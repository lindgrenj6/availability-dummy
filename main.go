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
	ctx = context.Background()
	// clowder config
	cfg = clowder.LoadedConfig
	// kafka writer
	k = &kafka.Writer{}
	// cost/subswatch IDs for foreign key lookups.
	costID   string
	swatchID string
)

func main() {
	if !clowder.IsClowderEnabled() {
		fmt.Fprintln(os.Stderr, "Clowder not enabled - exiting")
		os.Exit(1)
	}
	// get the kafka write ready
	setupKafka()
	// reach out to sources one time to get the list of application types
	getApplicationTypes()

	e := echo.New()
	e.Logger.SetLevel(log.DEBUG)
	e.Use(middleware.Logger())
	// for healthcheck
	e.GET("/healthz", func(c echo.Context) error { return c.String(http.StatusOK, "OK") })

	/*
		These two handlers closely imitate how we connect to cost + subswatch. Basically
		we post with an x-rh-identity and a payload like `{"source_id": "1234"}` and the
		application looks up the application of it's type associated with that source, checks
		it, and then posts a message on the kafka queue for our listener to pick up.

		The same logic can be used for both minus knowing which application type was passed in.
	*/

	// Cost
	e.POST("/api/cost-management/v1/source-status/", func(c echo.Context) error {
		//
		sendBackRandomResponse(c, costID)
		return c.String(http.StatusAccepted, "OK")
	})
	// Swatch
	e.POST("/internal/api/cloudigrade/v1/availability_status", func(c echo.Context) error {
		sendBackRandomResponse(c, swatchID)
		return c.String(http.StatusAccepted, "OK")
	})

	e.Logger.Fatal(e.Start(":8000"))
}

// Steps inline, basically mocks what the actual apps do.
func sendBackRandomResponse(c echo.Context, appTypeId string) {
	// 1. Parses JSON to get Source ID
	body := make(map[string]interface{})
	err := c.Bind(&body)
	if err != nil {
		panic(err)
	}
	c.Logger().Debugf("%+v", body)

	// do the hard stuff async since this involves hitting sources + posting to kafka
	go func(c echo.Context, body map[string]interface{}, appTypeId string) {
		// 2. Grab x-rh-id
		xrhid := c.Request().Header.Get("x-rh-identity")
		// 3. Look up Application associated with the Source
		appID := getApplicationID(xrhid, body["source_id"].(string), appTypeId)
		// 4. Send an availability_status message back to sources, using same x-rh-id
		sendMessageToSources(randomStatusMessage(appID), []byte(xrhid))
	}(c, body, appTypeId)
}

// struct representing the message that gets sent on the kafka topic over to sources-api
// its so nice to have a struct rather than just a free-form hash!
type StatusMessage struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Status       string `json:"status"`
	Error        string `json:"error"`
}

// Generates a random status message for an application id
func randomStatusMessage(id string) []byte {
	msg := &StatusMessage{ResourceType: "application", ResourceID: id}

	// Flip a coin.
	// 	if heads: its available
	//  otherwise: unavailable, because "I have spoken."
	// TODO: hook into `fortune` on linux to spit out random reasons why
	if rand.Int()%2 == 0 {
		msg.Status = "available"
	} else {
		msg.Status = "unavailable"
		msg.Error = "I have spoken."
	}

	// unmarshal the message, panicing if it fails because it really shouldn't.
	out, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}

	return out
}

// Sends a message to sources with:
// headers:
//   event_type=availability_status
//   x-rh-identity=the one passed in
// body:
//   marshaled json `StatusMessage` struct
func sendMessageToSources(msg, xrhid []byte) {
	k.WriteMessages(ctx, kafka.Message{
		Headers: []kafka.Header{
			{Key: "x-rh-identity", Value: xrhid},
			{Key: "event_type", Value: []byte("availability_status")},
		},
		Value: msg,
	})
}

// Fetches an applicationID from a specific source.
func getApplicationID(xrhid, sourceID, appTypeID string) string {
	// construct a new client - with the default header being the x-rh-identity
	client, err := sources.NewAPIClient(xrhid)
	if err != nil {
		panic(err)
	}

	// GET /sources/:id/applications
	// TODO: handle errors. though it should "just work"
	applications, _, _ := client.DefaultApi.ListSourceApplications(ctx, sourceID).Execute()
	// iterate over the applications, returning the one that matches the application type
	// we passed in.
	for _, s := range *applications.Data {
		if *s.SourceId == sourceID && *s.ApplicationTypeId == appTypeID {
			return *s.Id
		}
	}

	// if we didn't find one that matches...how did this request even get made? better panic :P
	panic("could not find app id for source + type id")
}

// Lists all application types and sets the keys for us to compare against.
func getApplicationTypes() {
	// canned x-rh-id, looks like this: {"identity": {"account_number": "nil", "user": {"is_org_admin": true}}}
	client, err := sources.NewAPIClient("eyJpZGVudGl0eSI6IHsiYWNjb3VudF9udW1iZXIiOiAibmlsIiwgInVzZXIiOiB7ImlzX29yZ19hZG1pbiI6IHRydWV9fX0=")
	if err != nil {
		panic(err)
	}
	data, resp, _ := client.DefaultApi.ListApplicationTypes(ctx).Execute()
	// This is a failure that can't be tolerated - if we cannot hit sources to get
	// the application types we aren't going to have a good time when trying to look up
	// other things from sources-api.
	if resp.StatusCode != 200 {
		panic("Failed to fetch ApplicationTypes")
	}

	// iterate over the application types - extracting the 2 IDs that we care about
	// and store them in the global var list.
	for _, v := range *data.Data {
		if strings.HasSuffix(*v.Name, "cost-management") {
			costID = *v.Id
		}
		if strings.HasSuffix(*v.Name, "cloud-meter") {
			swatchID = *v.Id
		}
	}
}

// connect to kafka based on clowder config. A bit verbose, but its because
// clowder is more of a "request" model rather than "declare"
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

	// we need this topic in order to produce messages.
	if topic == "" {
		panic("did not find topic for platform.sources.status, not good!")
	}

	k = kafka.NewWriter(kafka.WriterConfig{
		Brokers: brokers,
		Topic:   topic,
	})
}
