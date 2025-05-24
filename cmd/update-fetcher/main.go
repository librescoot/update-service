package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/librescoot/update-service/pkg/config"
	"github.com/librescoot/update-service/pkg/download"
	"github.com/librescoot/update-service/pkg/redis"
)

type FetchRequest struct {
	URL      string `json:"url"`
	ReplyTo  string `json:"reply_to"`
	Checksum string `json:"checksum,omitempty"`
}

type FetchResponse struct {
	Success   bool   `json:"success"`
	FilePath  string `json:"file_path,omitempty"`
	Error     string `json:"error,omitempty"`
	RequestID string `json:"request_id"`
}

func main() {
	var (
		redisAddr    = flag.String("redis-addr", "localhost:6379", "Redis server address")
		redisPass    = flag.String("redis-pass", "", "Redis password")
		redisDB      = flag.Int("redis-db", 0, "Redis database number")
		downloadDir  = flag.String("download-dir", "/tmp/updates", "Directory to store downloaded files")
		queueName    = flag.String("queue", "fetch-requests", "Redis queue name for fetch requests")
	)
	flag.Parse()

	log.Printf("Starting update-fetcher service")
	log.Printf("Redis: %s (DB %d)", *redisAddr, *redisDB)
	log.Printf("Download directory: %s", *downloadDir)
	log.Printf("Queue: %s", *queueName)

	cfg := &config.Config{
		Redis: config.RedisConfig{
			Addr:     *redisAddr,
			Password: *redisPass,
			DB:       *redisDB,
		},
	}

	redisClient := redis.NewClient(cfg)
	if err := redisClient.Connect(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer redisClient.Close()

	downloadManager := download.NewManager(*downloadDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown gracefully
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Received shutdown signal")
		cancel()
	}()

	log.Println("Waiting for fetch requests...")
	
	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down fetcher service")
			return
		default:
			// Block for up to 5 seconds waiting for a request
			requestData, err := redisClient.BLPop(ctx, 5*time.Second, *queueName)
			if err != nil {
				if err == context.Canceled {
					return
				}
				log.Printf("Error popping from queue: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}

			if requestData == "" {
				// Timeout, continue to check for shutdown
				continue
			}

			var request FetchRequest
			if err := json.Unmarshal([]byte(requestData), &request); err != nil {
				log.Printf("Error unmarshaling request: %v", err)
				continue
			}

			log.Printf("Processing fetch request for URL: %s", request.URL)
			
			response := processFetchRequest(ctx, downloadManager, request)
			
			// Send response back if ReplyTo is specified
			if request.ReplyTo != "" {
				responseData, _ := json.Marshal(response)
				if err := redisClient.Publish(request.ReplyTo, string(responseData)); err != nil {
					log.Printf("Error publishing response: %v", err)
				}
			}
		}
	}
}

func processFetchRequest(ctx context.Context, dm *download.Manager, request FetchRequest) FetchResponse {
	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	filePath, err := dm.Download(downloadCtx, request.URL)
	if err != nil {
		log.Printf("Download failed for %s: %v", request.URL, err)
		return FetchResponse{
			Success: false,
			Error:   fmt.Sprintf("Download failed: %v", err),
		}
	}

	// Verify checksum if provided
	if request.Checksum != "" {
		if err := dm.VerifyChecksum(filePath, request.Checksum); err != nil {
			log.Printf("Checksum verification failed for %s: %v", filePath, err)
			// Clean up the downloaded file
			os.Remove(filePath)
			return FetchResponse{
				Success: false,
				Error:   fmt.Sprintf("Checksum verification failed: %v", err),
			}
		}
		log.Printf("Checksum verified for %s", filePath)
	}

	log.Printf("Successfully downloaded %s to %s", request.URL, filePath)
	return FetchResponse{
		Success:  true,
		FilePath: filePath,
	}
}