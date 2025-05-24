package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/librescoot/update-service/pkg/config"
	"github.com/librescoot/update-service/pkg/mender"
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

var Version string

func main() {
	var (
		redisAddr    = flag.String("redis-addr", "localhost:6379", "Redis server address")
		redisPass    = flag.String("redis-pass", "", "Redis password")
		redisDB      = flag.Int("redis-db", 0, "Redis database number")
		updateKey    = flag.String("update-key", "", "Redis key to watch for updates (contains URL#checksum)")
		failureKey   = flag.String("failure-key", "", "Redis key for failure messages")
		component    = flag.String("component", "", "Component name (dbc/mdb)")
		updateType   = flag.String("update-type", "non-blocking", "Update type (blocking/non-blocking)")
		fetchQueue   = flag.String("fetch-queue", "fetch-requests", "Redis queue for fetch requests")
	)
	flag.Parse()

	if *updateKey == "" || *component == "" {
		log.Fatal("update-key and component are required")
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	if Version == "" {
		Version = "dev"
	}
	log.Printf("Update installer %s starting for component %s", Version, *component)

	if err := checkMenderAvailable(); err != nil {
		log.Fatalf("Error checking mender-update: %v", err)
	}

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

	menderClient := mender.NewClient()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Set initial status
	statusKey := fmt.Sprintf("mender/status/%s", *component)
	updateTypeKey := fmt.Sprintf("mender/update_type/%s", *component)
	
	if err := redisClient.HSet(statusKey, "status", "initializing"); err != nil {
		log.Printf("Error setting initial status: %v", err)
	}
	if err := redisClient.HSet(updateTypeKey, "update_type", *updateType); err != nil {
		log.Printf("Error setting initial update type: %v", err)
	}

	if err := checkAndCommitUpdate(menderClient); err != nil {
		log.Printf("Error checking/committing update: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Context canceled, exiting...")
			if err := redisClient.HSet(statusKey, "status", "unknown"); err != nil {
				log.Printf("Error setting final status: %v", err)
			}
			if err := redisClient.HSet(updateTypeKey, "update_type", "none"); err != nil {
				log.Printf("Error setting final update type: %v", err)
			}
			return
		default:
			if err := redisClient.HSet(statusKey, "status", "checking-updates"); err != nil {
				log.Printf("Error setting status to checking-updates: %v", err)
			}

			urlAndChecksum, err := waitForUpdate(ctx, redisClient, *updateKey)
			if err != nil {
				if err == context.Canceled {
					return
				}
				log.Printf("Error waiting for update: %v", err)
				if err := redisClient.HSet(statusKey, "status", "checking-update-error"); err != nil {
					log.Printf("Error setting error status: %v", err)
				}
				time.Sleep(5 * time.Second)
				continue
			}

			// Parse URL#checksum format
			parts := strings.Split(urlAndChecksum, "#")
			url := parts[0]
			var checksum string
			if len(parts) > 1 {
				checksum = parts[1]
			}

			log.Printf("Received update URL: %s", url)
			if checksum != "" {
				log.Printf("Checksum: %s", checksum)
			}

			if err := handleUpdate(ctx, url, checksum, redisClient, menderClient, *failureKey, statusKey, updateTypeKey, *fetchQueue, *updateType); err != nil {
				log.Printf("Error handling update: %v", err)
				status := "unknown"
				if strings.Contains(err.Error(), "download") {
					status = "downloading-update-error"
				} else if strings.Contains(err.Error(), "install") {
					status = "installing-update-error"
				}
				if err := redisClient.HSet(statusKey, "status", status); err != nil {
					log.Printf("Error setting error status: %v", err)
				}
				if *failureKey != "" {
					if err := redisClient.HSet(*failureKey, "error", err.Error()); err != nil {
						log.Printf("Error setting failure: %v", err)
					}
				}
			} else {
				if err := redisClient.HSet(statusKey, "status", "installation-complete-waiting-reboot"); err != nil {
					log.Printf("Error setting success status: %v", err)
				}
				if err := redisClient.HSet(updateTypeKey, "update_type", "none"); err != nil {
					log.Printf("Error setting update type to none: %v", err)
				}
				
				log.Println("Update installed successfully. Waiting for reboot...")
				select {
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func checkMenderAvailable() error {
	_, err := exec.LookPath("mender-update")
	if err != nil {
		return fmt.Errorf("mender-update not found in PATH: %w", err)
	}
	return nil
}

func checkAndCommitUpdate(menderClient *mender.Client) error {
	needsCommit, err := menderClient.NeedsCommit()
	if err != nil {
		return fmt.Errorf("error checking if update needs commit: %w", err)
	}

	if needsCommit {
		log.Println("Update needs to be committed, committing...")
		if err := menderClient.Commit(); err != nil {
			return fmt.Errorf("error committing update: %w", err)
		}
		log.Println("Update committed successfully")
	} else {
		log.Println("No update needs to be committed")
	}

	return nil
}

func waitForUpdate(ctx context.Context, redisClient *redis.Client, updateKey string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			url, err := redisClient.Get(updateKey)
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}
			if url != "" {
				return url, nil
			}
			time.Sleep(5 * time.Second)
		}
	}
}

func handleUpdate(
	ctx context.Context,
	url, checksum string,
	redisClient *redis.Client,
	menderClient *mender.Client,
	failureKey, statusKey, updateTypeKey string,
	fetchQueue, updateType string,
) error {
	var filePath string
	
	// Check if this is a file:// URL
	if strings.HasPrefix(url, "file://") {
		filePath = strings.TrimPrefix(url, "file://")
		log.Printf("Using local file: %s", filePath)
	} else {
		// Use fetcher service for downloads
		if err := redisClient.HSet(statusKey, "status", "downloading-updates"); err != nil {
			log.Printf("Error setting status to downloading-updates: %v", err)
		}

		var err error
		filePath, err = requestDownload(ctx, redisClient, url, checksum, fetchQueue)
		if err != nil {
			if err := redisClient.HSet(statusKey, "status", "downloading-update-error"); err != nil {
				log.Printf("Error setting status to downloading-update-error: %v", err)
			}
			return fmt.Errorf("error downloading update: %w", err)
		}
		log.Printf("Downloaded update to: %s", filePath)
	}

	log.Println("Installing update...")
	if err := redisClient.HSet(statusKey, "status", "installing-updates"); err != nil {
		log.Printf("Error setting status to installing-updates: %v", err)
	}

	if err := menderClient.Install(filePath); err != nil {
		// Clean up downloaded file on error
		if !strings.HasPrefix(url, "file://") {
			os.Remove(filePath)
		}
		if err := redisClient.HSet(statusKey, "status", "installing-update-error"); err != nil {
			log.Printf("Error setting status to installing-update-error: %v", err)
		}
		return fmt.Errorf("error installing update: %w", err)
	}
	log.Println("Update installed successfully")

	// Clean up downloaded file after successful install
	if !strings.HasPrefix(url, "file://") {
		if err := os.Remove(filePath); err != nil {
			log.Printf("Warning: Failed to remove downloaded file %s: %v", filePath, err)
		}
	}

	// Set final success status based on update type
	successStatus := "installation-complete-waiting-reboot"
	if updateType == "blocking" {
		successStatus = "installation-complete-waiting-dashboard-reboot"
	}
	if err := redisClient.HSet(statusKey, "status", successStatus); err != nil {
		log.Printf("Error setting final success status: %v", err)
	}

	return nil
}

func requestDownload(ctx context.Context, redisClient *redis.Client, url, checksum, fetchQueue string) (string, error) {
	
	// Create unique reply channel
	replyChannel := fmt.Sprintf("fetch-reply-%d", time.Now().UnixNano())
	
	// Create fetch request
	request := FetchRequest{
		URL:     url,
		ReplyTo: replyChannel,
		Checksum: checksum,
	}
	
	requestData, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("error marshaling fetch request: %w", err)
	}
	
	// Send request to fetcher
	if err := redisClient.RPush(fetchQueue, string(requestData)); err != nil {
		return "", fmt.Errorf("error sending fetch request: %w", err)
	}
	
	// Wait for response with timeout
	responseCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	
	responseData, err := redisClient.BLPop(responseCtx, 30*time.Minute, replyChannel)
	if err != nil {
		return "", fmt.Errorf("error waiting for fetch response: %w", err)
	}
	
	var response FetchResponse
	if err := json.Unmarshal([]byte(responseData), &response); err != nil {
		return "", fmt.Errorf("error unmarshaling fetch response: %w", err)
	}
	
	if !response.Success {
		return "", fmt.Errorf("fetch failed: %s", response.Error)
	}
	
	return response.FilePath, nil
}