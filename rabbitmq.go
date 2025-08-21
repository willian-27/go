package main

import (
	"os"
	"sync"

	"github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog/log"
)

var (
	rabbitConn    *amqp091.Connection
	rabbitChannel *amqp091.Channel
	rabbitEnabled bool
	rabbitOnce    sync.Once
	rabbitQueue   string
)

// Call this in main() or initialization
func InitRabbitMQ() {
	rabbitURL := os.Getenv("RABBITMQ_URL")
	rabbitQueue = os.Getenv("RABBITMQ_QUEUE")
	if rabbitQueue == "" {
		rabbitQueue = "whatsapp_events" // default queue
	}
	if rabbitURL == "" {
		rabbitEnabled = false
		log.Info().Msg("RABBITMQ_URL is not set. RabbitMQ publishing disabled.")
		return
	}
	var err error
	rabbitConn, err = amqp091.Dial(rabbitURL)
	if err != nil {
		rabbitEnabled = false
		log.Error().Err(err).Msg("Could not connect to RabbitMQ")
		return
	}
	rabbitChannel, err = rabbitConn.Channel()
	if err != nil {
		rabbitEnabled = false
		log.Error().Err(err).Msg("Could not open RabbitMQ channel")
		return
	}
	rabbitEnabled = true
	log.Info().
		Str("queue", rabbitQueue).
		Msg("RabbitMQ connection established.")
}

// Optionally, allow overriding the queue per message
func PublishToRabbit(data []byte, queueOverride ...string) error {
	if !rabbitEnabled {
		return nil
	}
	queueName := rabbitQueue
	if len(queueOverride) > 0 && queueOverride[0] != "" {
		queueName = queueOverride[0]
	}
	// Declare queue (idempotent)
	_, err := rabbitChannel.QueueDeclare(
		queueName,
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		log.Error().Err(err).Str("queue", queueName).Msg("Could not declare RabbitMQ queue")
		return err
	}
	err = rabbitChannel.Publish(
		"",        // exchange (default)
		queueName, // routing key = queue
		false,     // mandatory
		false,     // immediate
		amqp091.Publishing{
			ContentType: "application/json",
			Body:        data,
		},
	)
	if err != nil {
		log.Error().Err(err).Str("queue", queueName).Msg("Could not publish to RabbitMQ")
	} else {
		log.Debug().Str("queue", queueName).Msg("Published message to RabbitMQ")
	}
	return err
}

// Usage - like sendToGlobalWebhook
func sendToGlobalRabbit(jsonData []byte, queueName ...string) {
	if !rabbitEnabled {
		log.Debug().Msg("RabbitMQ publishing is disabled, not sending message")
		return
	}
	err := PublishToRabbit(jsonData, queueName...)
	if err != nil {
		log.Error().Err(err).Msg("Failed to publish to RabbitMQ")
	}
}
