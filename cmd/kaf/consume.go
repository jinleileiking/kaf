package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/Shopify/sarama"
	"github.com/birdayz/kaf/avro"
	prettyjson "github.com/hokaccha/go-prettyjson"
	colorable "github.com/mattn/go-colorable"
	"github.com/spf13/cobra"
)

var (
	offsetFlag  string
	raw         bool
	follow      bool
	schemaCache *avro.SchemaCache
	keyfmt      *prettyjson.Formatter
)

func init() {
	rootCmd.AddCommand(consumeCmd)
	consumeCmd.Flags().StringVar(&offsetFlag, "offset", "oldest", "Offset to start consuming. Possible values: oldest, newest.")
	consumeCmd.Flags().BoolVar(&raw, "raw", false, "Print raw output of messages, without key or prettified JSON")
	consumeCmd.Flags().BoolVarP(&follow, "follow", "f", false, "Shorthand to start consuming with offset HEAD-1 on each partition. Overrides --offset flag")

	keyfmt = prettyjson.NewFormatter()
	keyfmt.Newline = " " // Replace newline with space to avoid condensed output.
	keyfmt.Indent = 0
}

func getAvailableOffsetsRetry(
	ldr *sarama.Broker, req *sarama.OffsetRequest, d time.Duration,
) (*sarama.OffsetResponse, error) {
	var (
		err     error
		offsets *sarama.OffsetResponse
	)

	for {
		select {
		case <-time.After(d):
			return nil, err
		default:
			offsets, err = ldr.GetAvailableOffsets(req)
			if err == nil {
				return offsets, err
			}
		}
	}
}

const offsetsRetry = 500 * time.Millisecond

var consumeCmd = &cobra.Command{
	Use:   "consume",
	Short: "Consume messages",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {

		var offset int64
		switch offsetFlag {
		case "oldest":
			offset = sarama.OffsetOldest
		case "newest":
			offset = sarama.OffsetNewest
		default:
			// TODO: normally we would parse this to int64 but it's
			// difficult as we can have multiple partitions. need to
			// find a way to give offsets from CLI with a good
			// syntax.
			offset = sarama.OffsetNewest
		}
		topic := args[0]
		client := getClient()

		consumer, err := sarama.NewConsumerFromClient(client)
		if err != nil {
			errorExit("Unable to create consumer from client: %v\n", err)
		}

		partitions, err := consumer.Partitions(topic)
		if err != nil {
			errorExit("Unable to get partitions: %v\n", err)
		}

		schemaCache = getSchemaCache()

		wg := sync.WaitGroup{}
		mu := sync.Mutex{} // Synchronizes stderr and stdout.
		for _, partition := range partitions {

			wg.Add(1)

			go func(partition int32) {
				req := &sarama.OffsetRequest{
					Version: int16(1),
				}
				req.AddBlock(topic, partition, int64(-1), int32(0))
				ldr, err := client.Leader(topic, partition)
				if err != nil {
					errorExit("Unable to get leader: %v\n", err)
				}

				offsets, err := getAvailableOffsetsRetry(ldr, req, offsetsRetry)
				if err != nil {
					errorExit("Unable to get available offsets: %v\n", err)
				}
				followOffset := offsets.GetBlock(topic, partition).Offset - 1

				if follow && followOffset > 0 {
					offset = followOffset
					fmt.Fprintf(os.Stderr, "Starting on partition %v with offset %v\n", partition, offset)
				}

				pc, err := consumer.ConsumePartition(topic, partition, offset)
				if err != nil {
					errorExit("Unable to consume partition: %v\n", err)
				}

				for msg := range pc.Messages() {
					var stderr bytes.Buffer

					dataToDisplay, err := avroDecode(msg.Value)
					if err != nil {
						fmt.Fprintf(&stderr, "could not decode Avro data: %v\n", err)
					}

					if !raw {
						formatted, err := prettyjson.Format(dataToDisplay)
						if err == nil {
							dataToDisplay = formatted
						}

						w := tabwriter.NewWriter(&stderr, tabwriterMinWidth, tabwriterWidth, tabwriterPadding, tabwriterPadChar, tabwriterFlags)

						if len(msg.Headers) > 0 {
							fmt.Fprintf(w, "Headers:\n")
						}

						for _, hdr := range msg.Headers {
							var hdrValue string
							// Try to detect azure eventhub-specific encoding
							if len(hdr.Value) > 0 {
								switch hdr.Value[0] {
								case 161:
									hdrValue = string(hdr.Value[2 : 2+hdr.Value[1]])
								case 131:
									hdrValue = strconv.FormatUint(binary.BigEndian.Uint64(hdr.Value[1:9]), 10)
								default:
									hdrValue = string(hdr.Value)
								}
							}

							fmt.Fprintf(w, "\tKey: %v\tValue: %v\n", string(hdr.Key), hdrValue)

						}

						if msg.Key != nil && len(msg.Key) > 0 {
							key, err := avroDecode(msg.Key)
							if err != nil {
								fmt.Fprintf(&stderr, "could not decode Avro data: %v\n", err)
							}
							fmt.Fprintf(w, "Key:\t%v\n", formatKey(key))
						}
						fmt.Fprintf(w, "Partition:\t%v\nOffset:\t%v\nTimestamp:\t%v\n", msg.Partition, msg.Offset, msg.Timestamp)
						w.Flush()
					}

					mu.Lock()
					stderr.WriteTo(os.Stderr)
					colorable.NewColorableStdout().Write(dataToDisplay)
					fmt.Print("\n")
					mu.Unlock()
				}
				wg.Done()
			}(partition)
		}
		wg.Wait()

	},
}

func avroDecode(b []byte) ([]byte, error) {
	if schemaCache != nil {
		return schemaCache.DecodeMessage(b)
	}
	return b, nil
}

func formatKey(key []byte) string {
	b, err := keyfmt.Format(key)
	if err != nil {
		return string(key)
	}
	return string(b)
}
