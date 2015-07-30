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
	"fmt"
	"github.com/golang/protobuf/proto"
	mesos "github.com/mesos/mesos-go/mesosproto"
	util "github.com/mesos/mesos-go/mesosutil"
	"github.com/mesos/mesos-go/scheduler"
	"io/ioutil"
	"os"
	"os/signal"
)

type Scheduler struct {
	httpServer *HttpServer
	cluster    *Cluster
}

func (s *Scheduler) Start() error {
	Logger.Infof("Starting scheduler with configuration: \n%s", Config)

	ctrlc := make(chan os.Signal, 1)
	signal.Notify(ctrlc, os.Interrupt)

	if err := s.resolveDeps(); err != nil {
		return err
	}

	s.httpServer = NewHttpServer(Config.Api)
	go s.httpServer.Start()

	s.cluster = NewCluster()

	frameworkInfo := &mesos.FrameworkInfo{
		User:       proto.String(Config.User),
		Name:       proto.String(Config.FrameworkName),
		Role:       proto.String(Config.FrameworkRole),
		Checkpoint: proto.Bool(true),
	}

	driverConfig := scheduler.DriverConfig{
		Scheduler: s,
		Framework: frameworkInfo,
		Master:    Config.Master,
	}

	driver, err := scheduler.NewMesosSchedulerDriver(driverConfig)
	go func() {
		<-ctrlc
		s.Shutdown(driver)
	}()

	if err != nil {
		return fmt.Errorf("Unable to create SchedulerDriver: %s", err)
	}

	if stat, err := driver.Run(); err != nil {
		Logger.Infof("Framework stopped with status %s and error: %s\n", stat.String(), err)
		return err
	}

	//TODO stop http server

	return nil
}

func (s *Scheduler) Registered(driver scheduler.SchedulerDriver, id *mesos.FrameworkID, master *mesos.MasterInfo) {
	Logger.Infof("[Registered] framework: %s master: %s:%d", id.GetValue(), master.GetHostname(), master.GetPort())
}

func (s *Scheduler) Reregistered(driver scheduler.SchedulerDriver, master *mesos.MasterInfo) {
	Logger.Infof("[Reregistered] master: %s:%d", master.GetHostname(), master.GetPort())
}

func (s *Scheduler) Disconnected(scheduler.SchedulerDriver) {
	Logger.Info("[Disconnected]")
}

func (s *Scheduler) ResourceOffers(driver scheduler.SchedulerDriver, offers []*mesos.Offer) {
	Logger.Debugf("[ResourceOffers] %s", offers)

	for _, offer := range offers {
		declineReason := s.acceptOffer(driver, offer)
		if declineReason != "" {
			driver.DeclineOffer(offer.GetId(), &mesos.Filters{RefuseSeconds: proto.Float64(1)})
			Logger.Debugf("Declined offer: %s", declineReason)
		}
	}
}

func (s *Scheduler) OfferRescinded(driver scheduler.SchedulerDriver, id *mesos.OfferID) {
	Logger.Infof("[OfferRescinded] %s", id.GetValue())
}

func (s *Scheduler) StatusUpdate(driver scheduler.SchedulerDriver, status *mesos.TaskStatus) {
	Logger.Infof("[StatusUpdate] %s", status)
}

func (s *Scheduler) FrameworkMessage(driver scheduler.SchedulerDriver, executor *mesos.ExecutorID, slave *mesos.SlaveID, message string) {
	Logger.Infof("[FrameworkMessage] executor: %s slave: %s message: %s", executor, slave, message)
}

func (s *Scheduler) SlaveLost(driver scheduler.SchedulerDriver, slave *mesos.SlaveID) {
	Logger.Infof("[SlaveLost] %s", slave.GetValue())
}

func (s *Scheduler) ExecutorLost(driver scheduler.SchedulerDriver, executor *mesos.ExecutorID, slave *mesos.SlaveID, status int) {
	Logger.Infof("[ExecutorLost] executor: %s slave: %s status: %d", executor, slave, status)
}

func (s *Scheduler) Error(driver scheduler.SchedulerDriver, message string) {
	Logger.Errorf("[Error] %s", message)
}

func (s *Scheduler) Shutdown(driver *scheduler.MesosSchedulerDriver) {
	Logger.Info("Shutdown triggered, stopping driver")
	driver.Stop(false)
}

func (s *Scheduler) acceptOffer(driver scheduler.SchedulerDriver, offer *mesos.Offer) string {
	if s.cluster.Exists(offer.GetHostname()) {
		return fmt.Sprintf("Server on host %s is already running.", offer.GetHostname())
	} else {
		declineReason := s.match(offer)
		if declineReason == "" {
			s.launchTask(driver, offer)
		}
		return declineReason
	}
}

func (s *Scheduler) match(offer *mesos.Offer) string {
	if Config.Cpus > getScalarResources(offer, "cpus") {
		return "no cpus"
	}

	if Config.Mem > getScalarResources(offer, "mem") {
		return "no mem"
	}

	return ""
}

func (s *Scheduler) launchTask(driver scheduler.SchedulerDriver, offer *mesos.Offer) {
	taskName := fmt.Sprintf("statsd-kafka-%s", offer.GetHostname())
	taskId := &mesos.TaskID{
		Value: proto.String(fmt.Sprintf("%s-%s", taskName, uuid())),
	}

	task := &mesos.TaskInfo{
		Name:     proto.String(taskName),
		TaskId:   taskId,
		SlaveId:  offer.GetSlaveId(),
		Executor: s.createExecutor(offer.GetHostname()),
		Resources: []*mesos.Resource{
			util.NewScalarResource("cpus", Config.Cpus),
			util.NewScalarResource("mem", Config.Mem),
		},
	}

	s.cluster.Add(offer.GetHostname(), task)

	driver.LaunchTasks([]*mesos.OfferID{offer.GetId()}, []*mesos.TaskInfo{task}, &mesos.Filters{RefuseSeconds: proto.Float64(1)})
}

func (s *Scheduler) createExecutor(hostname string) *mesos.ExecutorInfo {
	id := fmt.Sprintf("statsd-kafka-%s", hostname)
	return &mesos.ExecutorInfo{
		ExecutorId: util.NewExecutorID(id),
		Name:       proto.String(id),
		Command: &mesos.CommandInfo{
			Value: proto.String(fmt.Sprintf("./%s --log.level %s", Config.Executor, Config.LogLevel)),
			Uris: []*mesos.CommandInfo_URI{
				&mesos.CommandInfo_URI{
					Value:      proto.String(fmt.Sprintf("%s/resource/%s", Config.Api, Config.Executor)),
					Executable: proto.Bool(true),
				},
			},
		},
	}
}

func (s *Scheduler) resolveDeps() error {
	files, _ := ioutil.ReadDir("./")
	for _, file := range files {
		if !file.IsDir() && executorMask.MatchString(file.Name()) {
			Config.Executor = file.Name()
		}
	}

	if Config.Executor == "" {
		return fmt.Errorf("%s not found in current dir", executorMask)
	}

	return nil
}

func getScalarResources(offer *mesos.Offer, resourceName string) float64 {
	resources := 0.0
	filteredResources := util.FilterResources(offer.Resources, func(res *mesos.Resource) bool {
		return res.GetName() == resourceName
	})
	for _, res := range filteredResources {
		resources += res.GetScalar().GetValue()
	}
	return resources
}
