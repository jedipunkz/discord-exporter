package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
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

// countChannelMessages counts all messages in a given channel
func countChannelMessages(session *discordgo.Session, channelID string) (int, error) {
	var lastMessageID string
	totalCount := 0

	for {
		messages, err := session.ChannelMessages(channelID, maxMessagesPerRequest, lastMessageID, "", "")
		if err != nil {
			return 0, fmt.Errorf("failed to get messages: %w", err)
		}

		messageCount := len(messages)
		totalCount += messageCount

		if messageCount < maxMessagesPerRequest {
			break
		}

		lastMessageID = messages[messageCount-1].ID
	}

	return totalCount, nil
}

// updateMessageCount fetches and updates the message count metrics for all channels
func updateMessageCount(session *discordgo.Session, config *Config) {
	channels, err := session.GuildChannels(config.ServerID)
	if err != nil {
		log.Printf("Failed to get guild channels: %v", err)
		return
	}

	for _, channel := range channels {
		// Skip excluded channels
		if _, excluded := config.ExcludedChannels[channel.Name]; excluded {
			log.Printf("Skipping excluded channel: %s", channel.Name)
			continue
		}

		totalCount, err := countChannelMessages(session, channel.ID)
		if err != nil {
			log.Printf("Failed to count messages for channel %s: %v", channel.Name, err)
			continue
		}

		messageCountGauge.WithLabelValues(channel.Name).Set(float64(totalCount))
		log.Printf("Channel %s: %d messages", channel.Name, totalCount)
	}
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
