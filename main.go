package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"github.com/labstack/gommon/log"
	clowder "github.com/redhatinsights/app-common-go/pkg/api/v1"
	"github.com/segmentio/kafka-go"
)

var ctx = context.Background()
var cfg = clowder.LoadedConfig
var k = &kafka.Writer{}

func main() {
	if !clowder.IsClowderEnabled() {
		fmt.Fprintln(os.Stderr, "Clowder not enabled - exiting")
		os.Exit(1)
	}
	setupKafka()

	e := echo.New()
	e.Logger.SetLevel(log.DEBUG)
	e.Use(middleware.Logger())

	// Cost
	e.POST("/api/cost-management/v1/source-status/", func(c echo.Context) error {
		body := make(map[string]interface{})
		err := c.Bind(&body)
		if err != nil {
			return c.String(http.StatusBadRequest, err.Error())
		}

		e.Logger.Infof("%+v", body)
		go send(randomStatusMessage("source", body["source_id"].(string)))
		return c.String(http.StatusOK, "OK")
	})

	// Swatch
	e.POST("/internal/api/cloudigrade/v1/availability_status", func(c echo.Context) error {
		body := make(map[string]interface{})
		err := c.Bind(&body)
		if err != nil {
			return c.String(http.StatusBadRequest, err.Error())
		}

		e.Logger.Infof("%+v", body)
		go send(randomStatusMessage("source", body["source_id"].(string)))
		return c.String(http.StatusOK, "OK")
	})

	e.Logger.Fatal(e.Start(":8000"))
}

type StatusMessage struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Status       string `json:"status"`
	Error        string `json:"error"`
}

func randomStatusMessage(class, id string) []byte {
	var msg *StatusMessage
	if rand.Int()%2 == 0 {
		msg = &StatusMessage{
			ResourceType: class,
			ResourceID:   id,
			Status:       "available",
		}
	} else {
		msg = &StatusMessage{
			ResourceType: class,
			ResourceID:   id,
			Status:       "unavailable",
			Error:        "I have spoken.",
		}
	}

	out, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}

	return out
}

func setupKafka() {
	brokers := make([]string, len(cfg.Kafka.Brokers))
	for i, broker := range cfg.Kafka.Brokers {
		brokers[i] = fmt.Sprintf("%s:%d", broker.Hostname, *broker.Port)
	}

	topic := ""
	for _, t := range cfg.Kafka.Topics {
		if t.RequestedName == "platform.sources.status" {
			topic = t.Name
		}
	}

	k = kafka.NewWriter(kafka.WriterConfig{
		Brokers: brokers,
		Topic:   topic,
	})
}

func send(msg []byte) {
	k.WriteMessages(ctx, kafka.Message{
		Headers: []kafka.Header{
			{Key: "x-rh-sources-account-number", Value: []byte("1460290")},
		},
		Value: msg,
	})
}
