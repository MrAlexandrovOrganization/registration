package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// InitTopics is an explicit operator command, never part of normal startup or tests.
func InitTopics(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	brokers := strings.Split(env("KAFKA_BROKERS", "kafka:9092"), ",")
	prefix := env("KAFKA_TOPIC_PREFIX", "registration.telegram")
	for _, r := range prefix {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return errors.New("invalid KAFKA_TOPIC_PREFIX")
		}
	}
	if len(prefix) < 3 || len(prefix) > 150 {
		return errors.New("invalid KAFKA_TOPIC_PREFIX")
	}
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return errors.New("Kafka unavailable")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	controller, err := conn.Controller()
	if err != nil {
		return errors.New("Kafka controller unavailable")
	}
	admin, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, fmt.Sprint(controller.Port)))
	if err != nil {
		return errors.New("Kafka controller connection failed")
	}
	defer admin.Close()
	_ = admin.SetDeadline(time.Now().Add(15 * time.Second))
	var topics []kafka.TopicConfig
	for _, kind := range []string{"broadcast"} {
		topics = append(topics, kafka.TopicConfig{Topic: prefix + "." + kind + ".v1", NumPartitions: 3, ReplicationFactor: 1, ConfigEntries: []kafka.ConfigEntry{{ConfigName: "retention.ms", ConfigValue: "604800000"}, {ConfigName: "retention.bytes", ConfigValue: "268435456"}, {ConfigName: "max.message.bytes", ConfigValue: "65536"}, {ConfigName: "min.insync.replicas", ConfigValue: "1"}}})
	}
	if err = admin.CreateTopics(topics...); err != nil {
		return errors.New("topic creation failed")
	}
	fmt.Println("registration topics ready; existing topic configuration is not overwritten")
	return nil
}
