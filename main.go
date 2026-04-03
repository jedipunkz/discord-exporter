package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
)

const (
	// Discord API limits
	maxMembersPerRequest  = 1000
	maxMessagesPerRequest = 100

	// Application settings
	defaultMetricsPort    = ":2112"
	defaultUpdateInterval = 15 * time.Minute
	maxConcurrentChannels = 5 // Maximum number of channels to process concurrently
)

// channelCache holds incremental counting state per channel
type channelCache struct {
	newestMessageID string
	totalCount      int
}

var (
	channelCacheMu sync.RWMutex
	channelCaches  = make(map[string]*channelCache)
)

var (
	memberCountGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "discord_members_count",
		Help: "Number of members in the Discord server",
	})
	messageCountGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "discord_message_count",
			Help: "Number of messages per channel",
		},
		[]string{"channel"},
	)
)

// Config holds the application configuration
type Config struct {
	Token            string
	ServerID         string
	ExcludedChannels map[string]struct{}
}

func init() {
	prometheus.MustRegister(memberCountGauge)
	prometheus.MustRegister(messageCountGauge)
}

// loadConfig reads and parses the configuration file
func loadConfig() (*Config, error) {
	viper.SetConfigName("discord-exporter")
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	config := &Config{
		Token:            viper.GetString("token"),
		ServerID:         viper.GetString("serverID"),
		ExcludedChannels: parseExcludedChannels(viper.GetString("excludeChannels")),
	}

	if config.Token == "" {
		return nil, fmt.Errorf("Discord token is required")
	}
	if config.ServerID == "" {
		return nil, fmt.Errorf("serverID is required")
	}

	return config, nil
}

// parseExcludedChannels parses comma-separated channel names into a map
func parseExcludedChannels(channelsStr string) map[string]struct{} {
	excluded := make(map[string]struct{})
	if channelsStr == "" {
		return excluded
	}

	channelNames := strings.Split(channelsStr, ",")
	for _, name := range channelNames {
		trimmedName := strings.TrimSpace(name)
		if trimmedName != "" {
			excluded[trimmedName] = struct{}{}
		}
	}
	return excluded
}

// updateMemberCount fetches and updates the member count metric
func updateMemberCount(session *discordgo.Session, serverID string) {
	members, err := session.GuildMembers(serverID, "", maxMembersPerRequest)
	if err != nil {
		log.Printf("Failed to get guild members: %v", err)
		return
	}

	memberCount := len(members)
	memberCountGauge.Set(float64(memberCount))
	log.Printf("Member count: %d", memberCount)
}

// countChannelMessages counts messages in a channel using incremental counting.
// On the first call it fetches all messages and caches the newest message ID.
// On subsequent calls it only fetches messages newer than the cached ID.
func countChannelMessages(session *discordgo.Session, channelID string) (int, error) {
	channelCacheMu.RLock()
	cache, hasCached := channelCaches[channelID]
	channelCacheMu.RUnlock()

	if hasCached {
		return countNewMessages(session, channelID, cache)
	}
	return countAllMessages(session, channelID)
}

// countAllMessages fetches all messages in a channel from scratch and populates the cache.
func countAllMessages(session *discordgo.Session, channelID string) (int, error) {
	var beforeID string
	var newestMessageID string
	totalCount := 0

	for {
		messages, err := session.ChannelMessages(channelID, maxMessagesPerRequest, beforeID, "", "")
		if err != nil {
			return 0, fmt.Errorf("failed to get messages: %w", err)
		}

		messageCount := len(messages)
		if messageCount == 0 {
			break
		}

		// First batch: messages[0] is the newest message overall
		if newestMessageID == "" {
			newestMessageID = messages[0].ID
		}

		totalCount += messageCount

		if messageCount < maxMessagesPerRequest {
			break
		}

		beforeID = messages[messageCount-1].ID
	}

	if newestMessageID != "" {
		channelCacheMu.Lock()
		channelCaches[channelID] = &channelCache{
			newestMessageID: newestMessageID,
			totalCount:      totalCount,
		}
		channelCacheMu.Unlock()
	}

	return totalCount, nil
}

// countNewMessages fetches only messages newer than the cached message ID and adds them to the total.
func countNewMessages(session *discordgo.Session, channelID string, cache *channelCache) (int, error) {
	afterID := cache.newestMessageID
	newCount := 0
	var newestMessageID string

	for {
		// "after" returns messages in ascending order (oldest first in the batch)
		messages, err := session.ChannelMessages(channelID, maxMessagesPerRequest, "", afterID, "")
		if err != nil {
			return 0, fmt.Errorf("failed to get messages: %w", err)
		}

		messageCount := len(messages)
		if messageCount == 0 {
			break
		}

		// The last message in the batch is the newest
		newestMessageID = messages[messageCount-1].ID
		newCount += messageCount

		if messageCount < maxMessagesPerRequest {
			break
		}

		afterID = newestMessageID
	}

	totalCount := cache.totalCount + newCount
	updatedNewest := cache.newestMessageID
	if newestMessageID != "" {
		updatedNewest = newestMessageID
	}

	channelCacheMu.Lock()
	channelCaches[channelID] = &channelCache{
		newestMessageID: updatedNewest,
		totalCount:      totalCount,
	}
	channelCacheMu.Unlock()

	return totalCount, nil
}

// channelCountResult holds the result of counting messages in a channel
type channelCountResult struct {
	channelName string
	count       int
	err         error
}

// processChannel processes a single channel and sends the result to the results channel
func processChannel(session *discordgo.Session, channel *discordgo.Channel, results chan<- channelCountResult) {
	totalCount, err := countChannelMessages(session, channel.ID)
	results <- channelCountResult{
		channelName: channel.Name,
		count:       totalCount,
		err:         err,
	}
}

// updateMessageCount fetches and updates the message count metrics for all channels
// Uses concurrent processing with a worker pool to improve performance
func updateMessageCount(session *discordgo.Session, config *Config) {
	startTime := time.Now()

	channels, err := session.GuildChannels(config.ServerID)
	if err != nil {
		log.Printf("Failed to get guild channels: %v", err)
		return
	}

	// Filter out excluded channels and non-text channels
	var activeChannels []*discordgo.Channel
	for _, channel := range channels {
		// Skip non-text channels (voice, category, etc.)
		if channel.Type != discordgo.ChannelTypeGuildText {
			continue
		}

		if _, excluded := config.ExcludedChannels[channel.Name]; excluded {
			log.Printf("Skipping excluded channel: %s", channel.Name)
			continue
		}
		activeChannels = append(activeChannels, channel)
	}

	if len(activeChannels) == 0 {
		log.Println("No active channels to process")
		return
	}

	log.Printf("Processing %d channels concurrently (max %d workers)", len(activeChannels), maxConcurrentChannels)

	// Create channels for communication
	results := make(chan channelCountResult, len(activeChannels))
	semaphore := make(chan struct{}, maxConcurrentChannels)
	var wg sync.WaitGroup

	// Process channels concurrently
	for _, channel := range activeChannels {
		wg.Add(1)
		go func(ch *discordgo.Channel) {
			defer wg.Done()

			// Acquire semaphore
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			processChannel(session, ch, results)
		}(channel)
	}

	// Wait for all goroutines to complete and close results channel
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	successCount := 0
	errorCount := 0
	for result := range results {
		if result.err != nil {
			log.Printf("Failed to count messages for channel %s: %v", result.channelName, result.err)
			errorCount++
			continue
		}

		messageCountGauge.WithLabelValues(result.channelName).Set(float64(result.count))
		log.Printf("Channel %s: %d messages", result.channelName, result.count)
		successCount++
	}

	elapsed := time.Since(startTime)
	log.Printf("Message count update completed in %v (%d successful, %d errors)", elapsed, successCount, errorCount)
}

// startMetricsCollector starts a goroutine that periodically updates metrics
func startMetricsCollector(session *discordgo.Session, config *Config) {
	go func() {
		for {
			updateMemberCount(session, config.ServerID)
			updateMessageCount(session, config)
			time.Sleep(defaultUpdateInterval)
		}
	}()
}

func main() {
	// Load configuration
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Configuration loaded successfully. Server ID: %s", config.ServerID)
	log.Printf("Excluded channels: %d", len(config.ExcludedChannels))

	// Initialize Discord session
	session, err := discordgo.New("Bot " + config.Token)
	if err != nil {
		log.Fatalf("Failed to create Discord session: %v", err)
	}
	log.Println("Discord session created successfully")

	// Start metrics collection
	startMetricsCollector(session, config)
	log.Printf("Metrics collector started (update interval: %v)", defaultUpdateInterval)

	// Start HTTP server for Prometheus metrics
	http.Handle("/metrics", promhttp.Handler())
	log.Printf("Starting HTTP server on %s", defaultMetricsPort)
	if err := http.ListenAndServe(defaultMetricsPort, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}
