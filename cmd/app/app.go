package app

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/pyto86pri/mackerel-agent-lambda/cmd/agent"
	"github.com/pyto86pri/mackerel-agent-lambda/cmd/extensions"
	"github.com/pyto86pri/mackerel-agent-lambda/cmd/libs"
	"github.com/pyto86pri/mackerel-agent-lambda/cmd/metrics"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mackerelio/mackerel-client-go"
	log "github.com/sirupsen/logrus"
)

// App ...
type App struct {
	MackerelClient   *mackerel.Client
	ExtensionsClient *extensions.Client
	Agent            *agent.Agent
	Bucket           *metrics.ValuesBucket

	hostID string
}

func getAccountID() (string, error) {
	sess := session.Must(session.NewSession())
	svc := sts.New(sess)
	id, err := svc.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return *id.Account, nil
}

func (app *App) init() (err error) {
	err = app.MackerelClient.CreateGraphDefs([]*mackerel.GraphDefsParam{
		metrics.CPUGraphDefs,
		metrics.LoadavgGraphDefs,
		metrics.MemoryGraphDefs,
		metrics.NetworkGraphDefs,
	})
	if err != nil {
		return
	}
	accountID, err := getAccountID()
	if err != nil {
		return
	}
	region := os.Getenv("AWS_REGION")
	functionName := os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	functionArn := fmt.Sprintf("arn:aws:lambda:%s:%s:function:%s", region, accountID, functionName)
	hosts, err := app.MackerelClient.FindHosts(&mackerel.FindHostsParam{
		CustomIdentifier: functionArn,
		Statuses:         []string{"working", "standby", "maintenance", "poweroff"},
	})
	if err != nil {
		return
	}
	if len(hosts) > 1 {
		err = fmt.Errorf("Custom identifier duplicated")
		return
	}
	if len(hosts) == 0 {
		app.hostID, err = app.MackerelClient.CreateHost(&mackerel.CreateHostParam{
			Name:             functionName,
			CustomIdentifier: functionArn,
		})
		if err != nil {
			return
		}
	} else {
		app.hostID = hosts[0].ID
	}
	return

}

func (app *App) collectMetrics(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			go app.Agent.Collect(app.Bucket)
		case <-ctx.Done():
			return
		}
	}
}

func (app *App) sendMetrics(now int64, values *metrics.Values) (err error) {
	if len(*values) == 0 {
		return
	}
	var metricValues []*mackerel.HostMetricValue
	for name, value := range *values {
		metricValues = append(metricValues, &mackerel.HostMetricValue{
			HostID: app.hostID,
			MetricValue: &mackerel.MetricValue{
				Name:  name,
				Time:  now,
				Value: value,
			},
		})
	}
	err = app.MackerelClient.PostHostMetricValues(metricValues)
	return
}

func (app *App) flushOnce() {
	now := time.Now().Unix()
	// Skip if not one hour passed after last flushing
	if app.Bucket.LastFlushedAt()+60 > now {
		return
	}
	// TODO: Switch reduce fn depending on metrics
	values := libs.MapReduce(app.Bucket.Flush(), math.Max)
	err := app.sendMetrics(now, values)
	if err != nil {
		log.Error(err)
	}
}

func (app *App) flush(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			app.flushOnce()
		case <-ctx.Done():
			app.flushOnce()
			return
		}
	}
}

func (app *App) loop() {
	for {
		event, err := app.ExtensionsClient.Next()
		if err != nil {
			continue
		}
		switch event.EventType {
		case "INVOKE":
			app.flushOnce()
		case "SHUTDOWN":
			return
		default:
			log.Warning("Unknown event type")
		}
	}
}

// Run run application
func (app *App) Run(ctx context.Context) {
	_, err := app.ExtensionsClient.Register()
	if err != nil {
		log.Fatal("Failed to register")
	}
	err = app.init()
	if err != nil {
		app.ExtensionsClient.InitError(&extensions.ErrorRequest{})
		log.Fatal("Failed to initialize")
	}
	ctx, cancel := context.WithCancel(ctx)
	go app.collectMetrics(ctx, 1*time.Second)
	go app.flush(ctx, 60*time.Second)
	app.loop()
	cancel()
	os.Exit(0)
}
