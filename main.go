package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/viper"
)

var (
	discordSession   *discordgo.Session
	serverID         string
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

func init() {
	viper.SetConfigName("discord-exporter")
	viper.AddConfigPath(".")
	err := viper.ReadInConfig()
	if err != nil {
		log.Fatalf("Error reading config file, %s", err)
	}

	prometheus.MustRegister(memberCountGauge)
	prometheus.MustRegister(messageCountGauge)
}

func updateMemberCount(discordSession *discordgo.Session, serverID string) {
	members, err := discordSession.GuildMembers(serverID, "", 1000)
	if err != nil {
		log.Fatalf("Failed to get guild members: %v", err)
		return
	}

	memberCount := len(members)
	memberCountGauge.Set(float64(memberCount))
	log.Printf("Member count: %v", float64(memberCount))
}

func updateMessageCount(discordSession *discordgo.Session, serverID string) {
	channels, err := discordSession.GuildChannels(serverID)
	if err != nil {
		log.Fatalf("Failed to get guild channels: %v", err)
		return
	}

	// 除外するチャンネル名のリスト
	excludedChannels := map[string]struct{}{
		"パダワン部屋": {},
		"入室通知":   {},
		// 他のチャンネル名を追加...
	}

	for _, channel := range channels {
		// チャンネルが除外リストに含まれている場合、次のチャンネルへ
		if _, excluded := excludedChannels[channel.Name]; excluded {
			continue
		}

		var lastMessageID string
		totalMessageCount := 0

		for {
			messages, err := discordSession.ChannelMessages(channel.ID, 100, lastMessageID, "", "")
			if err != nil {
				log.Printf("Failed to get messages for channel %s: %v", channel.ID, err)
				break
			}

			messageCount := len(messages)
			totalMessageCount += messageCount

			if messageCount < 100 {
				break
			}

			lastMessageID = messages[messageCount-1].ID
		}

		messageCountGauge.WithLabelValues(channel.Name).Set(float64(totalMessageCount))
		log.Printf("Message count for channel %s: %v", channel.Name, float64(totalMessageCount))
	}
}

func main() {
	token := viper.GetString("token")
	serverID = viper.GetString("serverID")

	if token == "" {
		log.Println("No Discord token provided")
		os.Exit(1)
	}

	if serverID == "" {
		log.Println("No serverID provided")
		os.Exit(1)
	}

	var err error
	discordSession, err = discordgo.New("Bot " + token)
	if err != nil {
		log.Println(err)
	}

	go func() {
		for {
			updateMemberCount(discordSession, serverID)
			updateMessageCount(discordSession, serverID)
			time.Sleep(15 * time.Minute)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":2112", nil)
}
