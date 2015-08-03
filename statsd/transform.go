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
	"github.com/gogo/protobuf/proto"
	"github.com/stealthly/statsd-mesos-kafka/statsd/avro"
	pb "github.com/stealthly/statsd-mesos-kafka/statsd/proto"
)

const (
	TransformNone  = "none"
	TransformAvro  = "avro"
	TransformProto = "proto"
)

var transformFunctions map[string]func(string) interface{} = map[string]func(string) interface{}{
	TransformNone:  transformNone,
	TransformAvro:  transformAvro,
	TransformProto: transformProto,
}

func transformNone(message string) interface{} {
	return message
}

func transformAvro(message string) interface{} {
	logLine := avro.NewLogLine()
	logLine.Line = message

	return logLine
}

func transformProto(message string) interface{} {
	Logger.Info("proto transform")

	logLine := new(pb.LogLine) //TODO set logtypeid, source, timings
	logLine.Line = proto.String(message)

	serialized, err := proto.Marshal(logLine)
	if err != nil {
		Logger.Errorf("Proto marshal error: %s", err) //TODO what should we do?
	}
	return serialized
}
