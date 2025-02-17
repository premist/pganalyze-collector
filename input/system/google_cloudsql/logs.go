package google_cloudsql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub"
	"google.golang.org/api/option"

	"github.com/pganalyze/collector/config"
	"github.com/pganalyze/collector/logs"
	"github.com/pganalyze/collector/state"
	"github.com/pganalyze/collector/util"
)

type googleLogResource struct {
	ResourceType string            `json:"type"`
	Labels       map[string]string `json:"labels"`
}

type googleLogMessage struct {
	InsertID         string            `json:"insertId"`
	LogName          string            `json:"logName"`
	ReceiveTimestamp string            `json:"receiveTimestamp"`
	Resource         googleLogResource `json:"resource"`
	Severity         string            `json:"severity"`
	TextPayload      string            `json:"textPayload"`
	Timestamp        string            `json:"timestamp"`
}

type LogStreamItem struct {
	GcpProjectID          string
	GcpCloudSQLInstanceID string
	OccurredAt            time.Time
	Content               string
}

func setupPubSubSubscriber(ctx context.Context, wg *sync.WaitGroup, logger *util.Logger, config config.ServerConfig, gcpLogStream chan LogStreamItem) error {
	if strings.Count(config.GcpPubsubSubscription, "/") != 3 {
		return fmt.Errorf("Unsupported subscription format - must be \"projects/PROJECT_NAME/subscriptions/SUBSCRIPTION_NAME\", got: %s", config.GcpPubsubSubscription)
	}
	idParts := strings.SplitN(config.GcpPubsubSubscription, "/", 4)
	projectID := idParts[1]
	subID := idParts[3]

	var opts []option.ClientOption
	if config.GcpCredentialsFile != "" {
		logger.PrintVerbose("Using GCP credentials file located at: %s", config.GcpCredentialsFile)
		opts = append(opts, option.WithCredentialsFile(config.GcpCredentialsFile))
	} else {
		logger.PrintVerbose("No GCP credentials file provided; assuming GKE workload identity or VM-associated service account")
	}
	client, err := pubsub.NewClient(ctx, projectID, opts...)
	if err != nil {
		return fmt.Errorf("Failed to create Google PubSub client: %v", err)
	}

	sub := client.Subscription(subID)
	go func(ctx context.Context, wg *sync.WaitGroup, logger *util.Logger, sub *pubsub.Subscription) {
		wg.Add(1)
		for {
			logger.PrintVerbose("Initializing Google Pub/Sub handler")
			err := sub.Receive(ctx, func(ctx context.Context, pubsubMsg *pubsub.Message) {
				pubsubMsg.Ack()

				var msg googleLogMessage
				err = json.Unmarshal(pubsubMsg.Data, &msg)
				if err != nil {
					logger.PrintError("Error parsing JSON: %s", err)
					return
				}

				if msg.Resource.ResourceType != "cloudsql_database" && msg.Resource.ResourceType != "alloydb.googleapis.com/Instance" {
					return
				}
				if !strings.HasSuffix(msg.LogName, "postgres.log") {
					return
				}

				resourceContainer, ok := msg.Resource.Labels["resource_container"]
				if !ok || strings.Count(resourceContainer, "/") != 1 {
					return
				}
				parts := strings.SplitN(resourceContainer, "/", 2) // projects/project_id

				clusterID, ok := msg.Resource.Labels["cluster_id"]
				if !ok {
					return
				}

				t, _ := time.Parse(time.RFC3339Nano, msg.Timestamp)

				gcpLogStream <- LogStreamItem{
					GcpProjectID:          parts[1],
					GcpCloudSQLInstanceID: clusterID,
					Content:               msg.TextPayload,
					OccurredAt:            t,
				}
			})
			if err == nil || err == context.Canceled {
				break
			}

			logger.PrintError("Failed to receive from Google PubSub, retrying in 1 minute: %v", err)
			time.Sleep(1 * time.Minute)
		}
		wg.Done()
	}(ctx, wg, logger, sub)

	return nil
}

func SetupLogSubscriber(ctx context.Context, wg *sync.WaitGroup, globalCollectionOpts state.CollectionOpts, logger *util.Logger, servers []*state.Server, parsedLogStream chan state.ParsedLogStreamItem) error {
	gcpLogStream := make(chan LogStreamItem, state.LogStreamBufferLen)
	setupLogTransformer(ctx, wg, servers, gcpLogStream, parsedLogStream, logger)

	// This map is used to avoid duplicate receivers to the same subscriber
	gcpPubSubHandlers := make(map[string]bool)

	for _, server := range servers {
		prefixedLogger := logger.WithPrefix(server.Config.SectionName)
		if server.Config.GcpPubsubSubscription != "" {
			_, ok := gcpPubSubHandlers[server.Config.GcpPubsubSubscription]
			if ok {
				continue
			}
			err := setupPubSubSubscriber(ctx, wg, prefixedLogger, server.Config, gcpLogStream)
			if err != nil {
				if globalCollectionOpts.TestRun {
					return err
				}

				prefixedLogger.PrintWarning("Skipping logs, could not setup log subscriber: %s", err)
				continue
			}

			gcpPubSubHandlers[server.Config.GcpPubsubSubscription] = true
		}
	}

	return nil
}

func setupLogTransformer(ctx context.Context, wg *sync.WaitGroup, servers []*state.Server, in <-chan LogStreamItem, out chan state.ParsedLogStreamItem, logger *util.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Only ingest log lines that were written in the last minute before startup
		linesNewerThan := time.Now().Add(-1 * time.Minute)

		for {
			select {
			case <-ctx.Done():
				return
			case in, ok := <-in:
				if !ok {
					return
				}

				// Note that we need to restore the original trailing newlines since
				// ProcessLogStream below expects them and they are not present in the GCP
				// log stream.
				logLine, ok := logs.ParseLogLineWithPrefix("", in.Content+"\n")
				if !ok {
					logger.PrintError("Can't parse log line: \"%s\"", in.Content)
					continue
				}
				logLine.OccurredAt = in.OccurredAt

				// Ignore loglines which are outside our time window
				if !logLine.OccurredAt.IsZero() && logLine.OccurredAt.Before(linesNewerThan) {
					continue
				}

				for _, server := range servers {
					if in.GcpProjectID == server.Config.GcpProjectID && in.GcpCloudSQLInstanceID == server.Config.GcpCloudSQLInstanceID {
						out <- state.ParsedLogStreamItem{Identifier: server.Config.Identifier, LogLine: logLine}
					}
				}
			}
		}
	}()
}
