package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/prometheus/common/config"

	"github.com/grafana/loki/pkg/logcli/client"
	"github.com/grafana/loki/pkg/logcli/output"
	"github.com/grafana/loki/pkg/logcli/query"
)

var (
	lokiEndpoint string
	orgID        string
	since        time.Duration
)

func init() {
	defaultSince, _ := time.ParseDuration("120h")
	flag.StringVar(&lokiEndpoint, "loki-endpoint", "http://localhost:3100", "The loki endpoint to query against")
	flag.StringVar(&orgID, "org-id", "1", "The tenant org ID")
	flag.DurationVar(&since, "since", defaultSince, "The duration to query for logs")
	flag.Parse()
}

func main() {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})
	app.Use(logger.New())

	app.Get("/logs/:namespace/:pod/:container", func(c *fiber.Ctx) error {
		fmt.Printf("StartTime: %s\n", c.Query("startTime", ""))
		fmt.Printf("CompletionTime: %s\n", c.Query("completionTime", ""))
		parallelDuration, _ := time.ParseDuration("1h")
		q := &query.Query{
			Start:              time.Now().Add(-since),
			End:                time.Now(),
			Quiet:              true,
			NoLabels:           true,
			Limit:              0,
			BatchSize:          5000,
			ColoredOutput:      false,
			ParallelMaxWorkers: 3,
			ParallelDuration:   parallelDuration,
			Forward:            true,
			QueryString:        fmt.Sprintf(`{container="%s",namespace="%s",pod="%s"}`, c.Params("container"), c.Params("namespace"), c.Params("pod")),
		}

		out, err := output.NewLogOutput(c.Response().BodyWriter(), "raw", &output.LogOutputOptions{
			NoLabels:      q.NoLabels,
			ColoredOutput: q.ColoredOutput,
		})
		if err != nil {
			return err
		}

		q.DoQueryParallel(&client.DefaultClient{
			TLSConfig: config.TLSConfig{},
			OrgID:     orgID,
			Address:   lokiEndpoint,
		}, out, false)

		return nil
	})

	log.Fatal(app.Listen(":3000"))
}
