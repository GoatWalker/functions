package server

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/context"

	"encoding/json"

	"github.com/Sirupsen/logrus"
	"github.com/gin-gonic/gin"
	"github.com/iron-io/functions/api/models"
	"github.com/iron-io/functions/api/runner"
	titancommon "github.com/iron-io/titan/common"
	"github.com/satori/go.uuid"
)

func handleSpecial(c *gin.Context) {
	ctx := c.MustGet("ctx").(context.Context)
	log := titancommon.Logger(ctx)

	err := Api.UseSpecialHandlers(c)
	if err != nil {
		log.WithError(err).Errorln("Error using special handler!")
		// todo: what do we do here? Should probably return a 500 or something
	}
}

func handleRunner(c *gin.Context) {
	if strings.HasPrefix(c.Request.URL.Path, "/v1") {
		c.Status(http.StatusNotFound)
		return
	}

	ctx := c.MustGet("ctx").(context.Context)
	log := titancommon.Logger(ctx)

	reqID := uuid.NewV5(uuid.Nil, fmt.Sprintf("%s%s%d", c.Request.RemoteAddr, c.Request.URL.Path, time.Now().Unix())).String()
	c.Set("reqID", reqID) // todo: put this in the ctx instead of gin's

	log = log.WithFields(logrus.Fields{"request_id": reqID})

	var err error

	var payload []byte
	if c.Request.Method == "POST" || c.Request.Method == "PUT" {
		payload, err = ioutil.ReadAll(c.Request.Body)
	} else if c.Request.Method == "GET" {
		qPL := c.Request.URL.Query()["payload"]
		if len(qPL) > 0 {
			payload = []byte(qPL[0])
		}
	}

	if len(payload) > 0 {
		var emptyJSON map[string]interface{}
		if err := json.Unmarshal(payload, &emptyJSON); err != nil {
			log.WithError(err).Error(models.ErrInvalidJSON)
			c.JSON(http.StatusBadRequest, simpleError(models.ErrInvalidJSON))
			return
		}
	}

	log.WithField("payload", string(payload)).Debug("Got payload")

	appName := c.Param("app")
	if appName == "" {
		// check context, app can be added via special handlers
		a, ok := c.Get("app")
		if ok {
			appName = a.(string)
		}
	}
	// if still no appName, we gotta exit
	if appName == "" {
		log.WithError(err).Error(models.ErrAppsNotFound)
		c.JSON(http.StatusBadRequest, simpleError(models.ErrAppsNotFound))
		return
	}

	routePath := c.Param("route")
	if routePath == "" {
		routePath = c.Request.URL.Path
	}
	routeBasePath := "/" + strings.Split(routePath, "/")[1] // Format the route path splitting URL

	log.WithFields(logrus.Fields{"app": appName, "path": routePath, "basePath": routeBasePath}).Info("Finding route on datastore")

	app, err := Api.Datastore.GetApp(appName)
	if err != nil || app == nil {
		log.WithError(err).Error(models.ErrAppsNotFound)
		c.JSON(http.StatusNotFound, simpleError(models.ErrAppsNotFound))
		return
	}

	route, err := Api.Datastore.GetRoute(appName, routeBasePath)
	if err != nil || route == nil {
		log.WithError(err).Error(models.ErrRoutesList)
		c.JSON(http.StatusInternalServerError, simpleError(models.ErrRoutesList))
		return
	}

	log.WithField("routes", route).Debug("Got routes from datastore")
	log = log.WithFields(logrus.Fields{
		"app": appName, "route": route.Path, "image": route.Image, "request_id": reqID})

	// Request count metric
	metricBaseName := "server.handleRunner." + appName + "."
	runner.LogMetricCount(ctx, (metricBaseName + "requests"), 1)

	var stdout bytes.Buffer // TODO: should limit the size of this, error if gets too big. akin to: https://golang.org/pkg/io/#LimitReader
	stderr := runner.NewFuncLogger(appName, route.Path, route.Image, reqID)

	envVars := map[string]string{
		"METHOD":      c.Request.Method,
		"ROUTE":       route.Path,
		"PAYLOAD":     string(payload),
		"REQUEST_URL": c.Request.URL.String(),
	}

	// app config
	for k, v := range app.Config {
		envVars["CONFIG_"+strings.ToUpper(k)] = v
	}

	// route config
	for k, v := range route.Config {
		envVars["CONFIG_"+strings.ToUpper(k)] = v
	}

	// params
	log.Info("Looking params")
	if params, match := getRouteParams(route.Path, routePath); match {
		log.Info("Math params")
		for _, param := range params {
			log.WithField("params", param.Key).Info("Key")
			log.WithField("params", param.Value).Info("Value")
			envVars["PARAM_"+strings.ToUpper(param.Key)] = param.Value
		}
	}

	// headers
	for header, value := range c.Request.Header {
		envVars["HEADER_"+strings.ToUpper(header)] = strings.Join(value, " ")
	}

	cfg := &runner.Config{
		Image:   route.Image,
		Timeout: 30 * time.Second,
		ID:      reqID,
		AppName: appName,
		Stdout:  &stdout,
		Stderr:  stderr,
		Env:     envVars,
	}

	metricStart := time.Now()
	if result, err := Api.Runner.Run(c, cfg); err != nil {
		log.WithError(err).Error(models.ErrRunnerRunRoute)
		c.JSON(http.StatusInternalServerError, simpleError(models.ErrRunnerRunRoute))
	} else {
		for k, v := range route.Headers {
			c.Header(k, v[0])
		}

		if result.Status() == "success" {
			c.Data(http.StatusOK, "", stdout.Bytes())
			runner.LogMetricCount(ctx, (metricBaseName + "succeeded"), 1)

		} else {
			// log.WithFields(logrus.Fields{"app": appName, "route": el, "req_id": reqID}).Debug(stderr.String())
			// Error count metric
			runner.LogMetricCount(ctx, (metricBaseName + "error"), 1)

			c.AbortWithStatus(http.StatusInternalServerError)
		}
	}
	// Execution time metric
	metricElapsed := time.Since(metricStart)
	runner.LogMetricTime(ctx, (metricBaseName + "time"), metricElapsed)
	return
}

var fakeHandler = func(http.ResponseWriter, *http.Request, Params) {}

func getRouteParams(baseRoute, route string) (Params, bool) {
	tree := &node{}
	tree.addRoute(baseRoute, fakeHandler)
	handler, p, _ := tree.getValue(route)
	if handler == nil {
		return nil, false
	}

	return p, true
}
