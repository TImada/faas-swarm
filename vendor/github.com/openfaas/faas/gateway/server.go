// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	internalHandlers "github.com/openfaas/faas/gateway/handlers"
	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/plugin"
	"github.com/openfaas/faas/gateway/types"
	natsHandler "github.com/openfaas/nats-queue-worker/handler"
)

func main() {

	osEnv := types.OsEnv{}
	readConfig := types.ReadConfig{}
	config := readConfig.Read(osEnv)

	log.Printf("HTTP Read Timeout: %s", config.ReadTimeout)
	log.Printf("HTTP Write Timeout: %s", config.WriteTimeout)

	if !config.UseExternalProvider() {
		log.Fatalln("As of this version of OpenFaaS, you must use external provider even for Docker Swarm.")
	}

	log.Printf("Binding to external function provider: %s", config.FunctionsProviderURL)

	metricsOptions := metrics.BuildMetricsOptions()
	metrics.RegisterMetrics(metricsOptions)

	var faasHandlers types.HandlerSet

	servicePollInterval := time.Second * 5

	reverseProxy := types.NewHTTPClientReverseProxy(config.FunctionsProviderURL, config.ReadTimeout)

	faasHandlers.Proxy = internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)
	faasHandlers.RoutelessProxy = internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)
	faasHandlers.ListFunctions = internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)
	faasHandlers.DeployFunction = internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)
	faasHandlers.DeleteFunction = internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)
	faasHandlers.UpdateFunction = internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)

	queryFunction := internalHandlers.MakeForwardingProxyHandler(reverseProxy, &metricsOptions)

	alertHandler := plugin.NewExternalServiceQuery(*config.FunctionsProviderURL)
	faasHandlers.Alert = internalHandlers.MakeAlertHandler(alertHandler)

	metrics.AttachExternalWatcher(*config.FunctionsProviderURL, metricsOptions, "func", servicePollInterval)

	if config.UseNATS() {
		log.Println("Async enabled: Using NATS Streaming.")
		natsQueue, queueErr := natsHandler.CreateNatsQueue(*config.NATSAddress, *config.NATSPort)
		if queueErr != nil {
			log.Fatalln(queueErr)
		}

		faasHandlers.QueuedProxy = internalHandlers.MakeQueuedProxy(metricsOptions, true, natsQueue)
		faasHandlers.AsyncReport = internalHandlers.MakeAsyncReport(metricsOptions)
	}

	prometheusQuery := metrics.NewPrometheusQuery(config.PrometheusHost, config.PrometheusPort, &http.Client{})
	listFunctions := metrics.AddMetricsHandler(faasHandlers.ListFunctions, prometheusQuery)
	faasHandlers.Proxy = internalHandlers.MakeCallIDMiddleware(faasHandlers.Proxy)
	r := mux.NewRouter()

	// r.StrictSlash(false)	// This didn't work, so register routes twice.
	r.HandleFunc("/function/{name:[-a-zA-Z_0-9]+}", faasHandlers.Proxy)
	r.HandleFunc("/function/{name:[-a-zA-Z_0-9]+}/", faasHandlers.Proxy)

	r.HandleFunc("/system/alert", faasHandlers.Alert)

	r.HandleFunc("/system/function/{name:[-a-zA-Z_0-9]+}", queryFunction).Methods("GET")
	r.HandleFunc("/system/functions", listFunctions).Methods("GET")
	r.HandleFunc("/system/functions", faasHandlers.DeployFunction).Methods("POST")
	r.HandleFunc("/system/functions", faasHandlers.DeleteFunction).Methods("DELETE")
	r.HandleFunc("/system/functions", faasHandlers.UpdateFunction).Methods("PUT")

	if faasHandlers.QueuedProxy != nil {
		r.HandleFunc("/async-function/{name:[-a-zA-Z_0-9]+}/", faasHandlers.QueuedProxy).Methods("POST")
		r.HandleFunc("/async-function/{name:[-a-zA-Z_0-9]+}", faasHandlers.QueuedProxy).Methods("POST")

		r.HandleFunc("/system/async-report", faasHandlers.AsyncReport)
	}

	fs := http.FileServer(http.Dir("./assets/"))

	// This URL allows access from the UI to the OpenFaaS store
	allowedCORSHost := "raw.githubusercontent.com"
	fsCORS := internalHandlers.DecorateWithCORS(fs, allowedCORSHost)

	r.PathPrefix("/ui/").Handler(http.StripPrefix("/ui", fsCORS)).Methods("GET")

	r.HandleFunc("/", faasHandlers.RoutelessProxy).Methods("POST")

	metricsHandler := metrics.PrometheusHandler()
	r.Handle("/metrics", metricsHandler)
	r.Handle("/", http.RedirectHandler("/ui/", http.StatusMovedPermanently)).Methods("GET")

	tcpPort := 8080

	s := &http.Server{
		Addr:           fmt.Sprintf(":%d", tcpPort),
		ReadTimeout:    config.ReadTimeout,
		WriteTimeout:   config.WriteTimeout,
		MaxHeaderBytes: http.DefaultMaxHeaderBytes, // 1MB - can be overridden by setting Server.MaxHeaderBytes.
		Handler:        r,
	}

	log.Fatal(s.ListenAndServe())
}
