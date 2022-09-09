package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"

	skafka "github.com/RedHatInsights/sources-api-go/kafka"
	sutil "github.com/RedHatInsights/sources-api-go/util"
	"github.com/labstack/echo"
	"github.com/labstack/echo/middleware"
	"github.com/labstack/gommon/log"
	clowder "github.com/redhatinsights/app-common-go/pkg/api/v1"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/protocol"
)

var (
	// clowder config
	cfg = clowder.LoadedConfig
	// kafka writer
	k *kafka.Writer
	// cost/subswatch IDs for foreign key lookups.
	costID   string
	swatchID string

	sourcesURL = fmt.Sprintf("%s://%s:%s/api/sources/v3.1", "http", os.Getenv("SOURCES_HOST"), os.Getenv("SOURCES_PORT"))
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
		msg := generateStatusMessage(&StatusMessage{ResourceID: appID, ResourceType: "Application"})
		c.Logger().Debugf("Sending [%s] for [%#v]", msg, body)
		sendMessageToSources(msg, []byte(xrhid))
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
func generateStatusMessage(msg *StatusMessage) []byte {
	switch strings.ToLower(os.Getenv("STATUS")) {
	case "available":
		msg.Status = "available"
	case "unavailable":
		msg.Status = "unavailable"
		msg.Error = "I have spoken."
	default:
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
	}

	// unmarshal the message, panicing if it fails because it really shouldn't.
	return must(json.Marshal(msg))
}

// Sends a message to sources with:
// headers:
//
//	event_type=availability_status
//	x-rh-identity=the one passed in
//
// body:
//
//	marshaled json `StatusMessage` struct
func sendMessageToSources(msg, xrhid []byte) {
	err := skafka.Produce(k, &skafka.Message{
		Headers: []protocol.Header{
			{Key: "x-rh-identity", Value: xrhid},
			{Key: "event_type", Value: []byte("availability_status")},
		},
		Value: msg,
	})
	if err != nil {
		fmt.Printf(`{"error": %v}\n`, err)
	}
}

// Fetches an applicationID from a specific source.
func getApplicationID(xrhid, sourceID, appTypeID string) string {
	// // GET /sources/:id/applications
	// // TODO: handle errors. though it should "just work"
	coll := getCollection("/sources/"+sourceID+"/applications", xrhid)

	// iterate over the applications, returning the one that matches the application type
	// we passed in.
	for _, s := range coll.Data {
		app := s.(map[string]any)
		if app["source_id"] == sourceID && app["application_type_id"] == appTypeID {
			return app["id"].(string)
		}
	}

	// if we didn't find one that matches...how did this request even get made? better panic :P
	panic("could not find app id for source + type id")
}

// Lists all application types and sets the keys for us to compare against.
func getApplicationTypes() {
	// This is a failure that can't be tolerated - if we cannot hit sources to get
	// the application types we aren't going to have a good time when trying to look up
	// other things from sources-api.
	coll := getCollection("/application_types", "")

	// iterate over the application types - extracting the 2 IDs that we care about
	// and store them in the global var list.
	for _, v := range coll.Data {
		at := v.(map[string]any)

		if strings.HasSuffix(at["name"].(string), "cost-management") {
			costID = at["id"].(string)
		}
		if strings.HasSuffix(at["name"].(string), "cloud-meter") {
			swatchID = at["id"].(string)
		}
	}
}

// connect to kafka based on clowder config. A bit verbose, but its because
// clowder is more of a "request" model rather than "declare"
func setupKafka() {
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

	k = must(skafka.GetWriter(&cfg.Kafka.Brokers[0], topic))
}

func getCollection(endpoint, xrhid string) *sutil.Collection {
	req := must(http.NewRequest(http.MethodGet, sourcesURL+endpoint, nil))
	if xrhid != "" {
		req.Header.Add("x-rh-identity", xrhid)
	}

	resp := must(http.DefaultClient.Do(req))
	if resp.StatusCode != 200 {
		panic("Failed to run request: " + endpoint)
	}

	bytes := must(io.ReadAll(resp.Body))
	coll := sutil.Collection{}
	json.Unmarshal(bytes, &coll)

	return &coll
}

func must[T any](thing T, err error) T {
	if err != nil {
		panic(err)
	}

	return thing
}
