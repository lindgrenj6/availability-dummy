package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"github.com/labstack/gommon/log"
	clowder "github.com/redhatinsights/app-common-go/pkg/api/v1"
	sources "github.com/redhatinsights/sources-superkey-worker/sources"
	"github.com/segmentio/kafka-go"
)

var (
	ctx      = context.Background()
	cfg      = clowder.LoadedConfig
	k        = &kafka.Writer{}
	costID   string
	swatchID string
)

func main() {
	if !clowder.IsClowderEnabled() {
		fmt.Fprintln(os.Stderr, "Clowder not enabled - exiting")
		os.Exit(1)
	}
	setupKafka()
	getApplicationTypes()

	e := echo.New()
	e.Logger.SetLevel(log.DEBUG)
	e.Use(middleware.Logger())
	e.GET("/healthz", func(c echo.Context) error { return c.String(http.StatusOK, "OK") })

	e.Logger.Debugf("Cost: %v; Swatch: %v\n", costID, swatchID)

	// Cost
	e.POST("/api/cost-management/v1/source-status/", func(c echo.Context) error {
		c.Set("type", costID)
		return reqHandler(c)
	})
	// Swatch
	e.POST("/internal/api/cloudigrade/v1/availability_status", func(c echo.Context) error {
		c.Set("type", swatchID)
		return reqHandler(c)
	})

	e.Logger.Fatal(e.Start(":8000"))
}

func reqHandler(c echo.Context) error {
	body := make(map[string]interface{})
	err := c.Bind(&body)
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}
	c.Logger().Debugf("%+v", body)

	xrhid := c.Request().Header.Get("x-rh-identity")
	appID := getApplicationID(xrhid, body["source_id"].(string), c.Get("type").(string))
	send(randomStatusMessage(appID), []byte(xrhid))
	return c.String(http.StatusOK, "OK")
}

type StatusMessage struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Status       string `json:"status"`
	Error        string `json:"error"`
}

func randomStatusMessage(id string) []byte {
	var msg *StatusMessage
	if rand.Int()%2 == 0 {
		msg = &StatusMessage{
			ResourceType: "application",
			ResourceID:   id,
			Status:       "available",
		}
	} else {
		msg = &StatusMessage{
			ResourceType: "application",
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

func send(msg, xrhid []byte) {
	k.WriteMessages(ctx, kafka.Message{
		Headers: []kafka.Header{
			{Key: "x-rh-identity", Value: xrhid},
			{Key: "event_type", Value: []byte("availability_status")},
		},
		Value: msg,
	})
}

func getApplicationID(xrhid, sourceID, appTypeID string) string {
	client, err := sources.NewAPIClient(xrhid)
	if err != nil {
		panic(err)
	}

	applications, _, _ := client.DefaultApi.ListApplications(ctx).Execute()
	for _, s := range *applications.Data {
		if *s.SourceId == sourceID && *s.ApplicationTypeId == appTypeID {
			return *s.Id
		}
	}
	panic("could not find app id for source + type id")
}

func getApplicationTypes() {
	// canned x-rh-id, looks like this: {"identity": {"account_number": "nil", "user": {"is_org_admin": true}}}
	client, err := sources.NewAPIClient("eyJpZGVudGl0eSI6IHsiYWNjb3VudF9udW1iZXIiOiAibmlsIiwgInVzZXIiOiB7ImlzX29yZ19hZG1pbiI6IHRydWV9fX0=")
	if err != nil {
		panic(err)
	}
	data, resp, _ := client.DefaultApi.ListApplicationTypes(ctx).Execute()
	if resp.StatusCode != 200 {
		panic("Failed to fetch ApplicationTypes")
	}

	for _, v := range *data.Data {
		if strings.HasSuffix(*v.Name, "cost-management") {
			costID = *v.Id
		}
		if strings.HasSuffix(*v.Name, "cloud-meter") {
			swatchID = *v.Id
		}
	}
}
