package main

import (
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	docopt "github.com/docopt/docopt-go"
	"github.com/jurriaan/kafkatools"
)

var (
	version     = "0.1"
	gitrev      = "unknown"
	versionInfo = `kt %s (git rev %s)`
	usage       = `kt - kafka cli tool

usage:
  kt consume --topic <topic> --broker <broker,..> [options]

options:
  -h --help                  show this screen.
  -V, --version              show version.
  -t, --topic <topic>        the topic
  -b, --broker <broker,..>   the brokers to connect to
  -o, --offset <offset>      offset to start consuming from: beginning | end | <value> (absolute offset) | -<value> (relative offset) TODO
  -p, --partition <n>        consume a single partition
	--start-date <timestamp>   start consuming from the specified timestamp
	--end-date <timestamp>     stop consuming until the specified timestamp
  -c, --count <n>            stop consuming after n messages
  -e, --exit                 stop consuming after the last message
`
)

func parseDateOpt(dateOpt interface{}) int64 {
	date, err := time.Parse(time.RFC3339, dateOpt.(string))
	if err != nil {
		log.Fatal("Invalid time format specified (RFC3339 required): ", err)
	}

	// Compute time in milliseconds
	return date.UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond))
}

type options struct {
	brokers     []string
	startOffset *int64
	endOffset   *int64
	partition   *int32
	topic       string
	count       int
}

type offsetMap map[int32]kafkatools.TopicPartitionOffset

func parseOptions() options {
	docOpts, err := docopt.Parse(usage, nil, true, fmt.Sprintf(versionInfo, version, gitrev), false)
	if err != nil {
		log.Panicf("[PANIC] We couldn't parse doc opts params: %v", err)
	}

	var startOffset, endOffset = new(int64), new(int64)
	*startOffset = sarama.OffsetNewest
	if docOpts["--start-date"] != nil {
		*startOffset = parseDateOpt(docOpts["--start-date"])
	}

	if docOpts["--end-date"] != nil {
		*endOffset = parseDateOpt(docOpts["--end-date"])
	} else if docOpts["--exit"].(bool) {
		*endOffset = sarama.OffsetNewest
	} else {
		endOffset = nil
	}

	count := -1
	if docOpts["--count"] != nil {
		count, err = strconv.Atoi(docOpts["--count"].(string))
		if err != nil {
			log.Fatal("Invalid count specified: ", err)
		}
	}

	var partition = new(int32)
	if docOpts["--partition"] != nil {
		if part, err := strconv.Atoi(docOpts["--partition"].(string)); err == nil {
			*partition = int32(part)
		} else {
			log.Fatal("Invalid partition specified: ", err)
		}
	} else {
		partition = nil
	}

	parsedOptions := options{
		brokers:     strings.Split(docOpts["--broker"].(string), ","),
		topic:       docOpts["--topic"].(string),
		startOffset: startOffset,
		endOffset:   endOffset,
		partition:   partition,
		count:       count,
	}

	return parsedOptions
}

func main() {
	var endOffsets offsetMap

	parsedOptions := parseOptions()
	client := kafkatools.GetSaramaClient(parsedOptions.brokers...)

	log.Println("Fetching offsets")
	partitionOffsets := kafkatools.FetchTopicOffsets(client, *parsedOptions.startOffset, parsedOptions.topic)

	if parsedOptions.partition != nil {
		val, found := partitionOffsets[*parsedOptions.partition]
		if !found {
			log.Fatalf("Partition %d not found for topic %s", *parsedOptions.partition, parsedOptions.topic)
		}

		partitionOffsets = make(offsetMap)
		partitionOffsets[val.Partition] = val
	}

	if parsedOptions.endOffset != nil {
		endOffsets = kafkatools.FetchTopicOffsets(client, *parsedOptions.endOffset, parsedOptions.topic)
	}

	consumer, err := sarama.NewConsumerFromClient(client)
	if err != nil {
		log.Fatalf("Could not start consumer: %v", err)
	}

	messages, closing := consumePartitions(consumer, partitionOffsets, endOffsets)

	printMessages(messages, parsedOptions.count, func(str string) { fmt.Println(str) })
	close(closing)

	if err = client.Close(); err != nil {
		log.Fatal("Could not properly close the client")
	}

	log.Println("Connection closed. Bye.")
}

func printMessages(messages chan *sarama.ConsumerMessage, maxMessages int, printer func(string)) {
	counter := 0

	for msg := range messages {
		printer(string(msg.Value))

		if maxMessages != -1 {
			counter++
			if counter >= maxMessages {
				log.Printf("Quiting after %d messages", counter)
				return
			}
		}
	}
}

func processErrors(pc sarama.PartitionConsumer) {
	for err := range pc.Errors() {
		log.Printf("error: we got an error while consuming one of the partitions: %v", err)
	}
}

func processMessages(pc sarama.PartitionConsumer, partitionEndOffset *int64, partitionCloser chan struct{}, messages chan *sarama.ConsumerMessage, wg *sync.WaitGroup) {
	defer wg.Done()
	for message := range pc.Messages() {
		if partitionEndOffset != nil {
			if message.Offset >= *partitionEndOffset {
				close(partitionCloser)
				break
			}
		}
		messages <- message
	}
}

func consumerCloser(pc io.Closer, partition int32, closing, partitionCloser chan struct{}) {
	select {
	case <-closing:
	case <-partitionCloser:
	}

	if err := pc.Close(); err != nil {
		log.Panicf("ERROR: Failed to close consumer for partition %d: %s", partition, err)
	}
}

func consumePartitions(consumer sarama.Consumer, partitionOffsets, endOffsets offsetMap) (messages chan *sarama.ConsumerMessage, closing chan struct{}) {
	var wg sync.WaitGroup
	messages = make(chan *sarama.ConsumerMessage)
	closing = make(chan struct{})

	for _, offset := range partitionOffsets {
		log.Printf("Consuming %s partition %d starting at %d (until %d)", offset.Topic, offset.Partition, offset.Offset, endOffsets[offset.Partition].Offset)
		pc, err := consumer.ConsumePartition(offset.Topic, offset.Partition, offset.Offset)
		if err != nil {
			log.Panicf("ERROR: Failed to start consumer for partition %d: %s", offset.Partition, err)
		}

		var partitionEndOffset *int64
		if endOffset, ok := endOffsets[offset.Partition]; ok {
			partitionEndOffset = new(int64)
			*partitionEndOffset = endOffset.Offset
		}
		partitionCloser := make(chan struct{})

		wg.Add(1)
		go consumerCloser(pc, offset.Partition, closing, partitionCloser)
		go processMessages(pc, partitionEndOffset, partitionCloser, messages, &wg)
		go processErrors(pc)
	}

	go func() {
		wg.Wait()
		if err := consumer.Close(); err != nil {
			log.Fatal("Error closing the consumer: ", err)
		}
		close(messages)
	}()

	return messages, closing
}
