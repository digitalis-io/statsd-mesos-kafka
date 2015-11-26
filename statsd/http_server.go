/* Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License. */

package statsd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	mesos "github.com/mesos/mesos-go/mesosproto"
	"strconv"
)

type HttpServer struct {
	address string
}

func NewHttpServer(address string) *HttpServer {
	if strings.HasPrefix(address, "http://") {
		address = address[len("http://"):]
	}
	return &HttpServer{
		address: address,
	}
}

func (hs *HttpServer) Start() {
	http.HandleFunc("/resource/", serveFile)
	http.HandleFunc("/api/start", handleStart)
	http.HandleFunc("/api/stop", handleStop)
	http.HandleFunc("/api/update", handleUpdate)
	http.HandleFunc("/api/status", handleStatus)
	http.ListenAndServe(hs.address, nil)
}

func serveFile(w http.ResponseWriter, r *http.Request) {
	resourceTokens := strings.Split(r.URL.Path, "/")
	resource := resourceTokens[len(resourceTokens)-1]
	http.ServeFile(w, r, resource)
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if Config.CanStart() {
		sched.SetActive(true)
		respond(true, "Servers started", w)
	} else {
		respond(false, "producer.properties and topic must be set before starting. schema.registry.url must be set for avro transform.", w)
	}
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	sched.SetActive(false)
	respond(true, "Servers stopped", w)
}

func handleUpdate(w http.ResponseWriter, r *http.Request) {
	queryParams := r.URL.Query()
	setConfig(queryParams, "producer.properties", &Config.ProducerProperties)
	setConfig(queryParams, "broker.list", &Config.BrokerList)
	setConfig(queryParams, "topic", &Config.Topic)
	setConfig(queryParams, "transform", &Config.Transform)
	setConfig(queryParams, "schema.registry.url", &Config.SchemaRegistryUrl)
	setFloatConfig(queryParams, "cpu", &Config.Cpus)
	setFloatConfig(queryParams, "mem", &Config.Mem)

	Logger.Infof("Scheduler configuration updated: \n%s", Config)
	respond(true, "Configuration updated", w)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	tasks := sched.cluster.GetAllTasks()
	response := "cluster:\n"
	for host, task := range tasks {
		response += fmt.Sprintf("  server: %s", host)
		response += fmt.Sprintf("    id: %s", task.GetTaskId())
		response += fmt.Sprintf("    slave id: %s", task.GetSlaveId())
		for _, resource := range task.GetResources() {
			switch *resource.Type {
			case mesos.Value_SCALAR:
				response += fmt.Sprintf("    %s: %s", resource.GetName(), resource.GetScalar())
			case mesos.Value_RANGES:
				response += fmt.Sprintf("    %s: %s", resource.GetName(), resource.GetRanges())
			case mesos.Value_SET:
				response += fmt.Sprintf("    %s: %s", resource.GetName(), resource.GetSet())
			}
		}
	}
	respond(true, response, w)
}

func setConfig(queryParams url.Values, name string, config *string) {
	value := queryParams.Get(name)
	if value != "" {
		*config = value
	}
}

func setFloatConfig(queryParams url.Values, name string, config *float64) {
	value := queryParams.Get(name)
	floatValue, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return
	}
	if value != "" {
		*config = floatValue
	}
}

func respond(success bool, message string, w http.ResponseWriter) {
	response := NewApiResponse(success, message)
	bytes, err := json.Marshal(response)
	if err != nil {
		panic(err) //this shouldn't happen
	}
	if success {
		w.WriteHeader(200)
	} else {
		w.WriteHeader(500)
	}
	w.Write(bytes)
}
