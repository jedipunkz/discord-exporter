# Discord Exporter

Discord Exporter is an exporter for retrieving the number of members in a Discord server and the number of messages in each channel via Prometheus.

## Features

- Retrieves the number of members in a Discord server
- Retrieves the number of messages in each channel

## Usage

1. Create a discord-exporter.yaml file and set your Discord token and server ID.

```
token: YOUR_DISCORD_TOKEN
serverID: YOUR_SERVER_ID
excludeChannels: CHANNEL_NAME,...
```
2. Use Docker-Compose to build and run the application.

```shell
docker-compose build
docker-compose up -d
```

3. Access http://localhost:2112/metrics in your browser to check the exported metrics.

## Metrics
- discord_members_count: The number of members in the Discord server
- discord_message_count: The number of messages in each channel
