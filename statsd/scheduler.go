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
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"sync"

	utils "github.com/elodina/go-mesos-utils"
	"github.com/golang/protobuf/proto"
	mesos "github.com/mesos/mesos-go/mesosproto"
	util "github.com/mesos/mesos-go/mesosutil"
	"github.com/mesos/mesos-go/scheduler"
)

var sched *Scheduler // This is needed for HTTP server to be able to update this scheduler

type Scheduler struct {
	httpServer *HttpServer
	cluster    *Cluster
	active     bool
	activeLock sync.Mutex
	driver     scheduler.SchedulerDriver
	labels     string
}

func (s *Scheduler) Start() error {
	Logger.Infof("Starting scheduler with configuration: \n%s", Config)
	sched = s // set this scheduler reachable for http server

	ctrlc := make(chan os.Signal, 1)
	signal.Notify(ctrlc, os.Interrupt)

	if err := s.resolveDeps(); err != nil {
		return err
	}

	listenAddr := s.listenAddr()
	s.httpServer = NewHttpServer(listenAddr)
	go s.httpServer.Start()

	s.cluster = NewCluster()
	s.labels = os.Getenv("STACK_LABELS")

	frameworkInfo := &mesos.FrameworkInfo{
		User:       proto.String(Config.User),
		Name:       proto.String(Config.FrameworkName),
		Role:       proto.String(Config.FrameworkRole),
		Checkpoint: proto.Bool(true),
		Labels:     utils.StringToLabels(s.labels),
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

func (s *Scheduler) SetActive(active bool) {
	s.activeLock.Lock()
	defer s.activeLock.Unlock()

	s.active = active
	if !s.active {
		for _, task := range s.cluster.GetAllTasks() {
			Logger.Debugf("Killing task %s", task.GetTaskId().GetValue())
			s.driver.KillTask(task.GetTaskId())
		}
	}
}

func (s *Scheduler) Registered(driver scheduler.SchedulerDriver, id *mesos.FrameworkID, master *mesos.MasterInfo) {
	Logger.Infof("[Registered] framework: %s master: %s:%d", id.GetValue(), master.GetHostname(), master.GetPort())

	s.driver = driver
}

func (s *Scheduler) Reregistered(driver scheduler.SchedulerDriver, master *mesos.MasterInfo) {
	Logger.Infof("[Reregistered] master: %s:%d", master.GetHostname(), master.GetPort())

	s.driver = driver
}

func (s *Scheduler) Disconnected(scheduler.SchedulerDriver) {
	Logger.Info("[Disconnected]")

	s.driver = nil
}

func (s *Scheduler) ResourceOffers(driver scheduler.SchedulerDriver, offers []*mesos.Offer) {
	Logger.Debugf("[ResourceOffers] %s", offersString(offers))

	s.activeLock.Lock()
	defer s.activeLock.Unlock()

	if !s.active {
		Logger.Debug("Scheduler is inactive. Declining all offers.")
		for _, offer := range offers {
			driver.DeclineOffer(offer.GetId(), &mesos.Filters{RefuseSeconds: proto.Float64(10)})
		}
		return
	}

	for _, offer := range offers {
		declineReason := s.acceptOffer(driver, offer)
		if declineReason != "" {
			driver.DeclineOffer(offer.GetId(), &mesos.Filters{RefuseSeconds: proto.Float64(10)})
			Logger.Debugf("Declined offer: %s", declineReason)
		}
	}
}

func (s *Scheduler) OfferRescinded(driver scheduler.SchedulerDriver, id *mesos.OfferID) {
	Logger.Infof("[OfferRescinded] %s", id.GetValue())
}

func (s *Scheduler) StatusUpdate(driver scheduler.SchedulerDriver, status *mesos.TaskStatus) {
	Logger.Infof("[StatusUpdate] %s", statusString(status))

	hostname := s.hostnameFromTaskId(status.GetTaskId().GetValue())

	if status.GetState() == mesos.TaskState_TASK_FAILED || status.GetState() == mesos.TaskState_TASK_KILLED ||
		status.GetState() == mesos.TaskState_TASK_LOST || status.GetState() == mesos.TaskState_TASK_ERROR ||
		status.GetState() == mesos.TaskState_TASK_FINISHED {
		s.cluster.Remove(hostname)
	}
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

	data, err := json.Marshal(Config)
	if err != nil {
		panic(err) //shouldn't happen
	}
	Logger.Debugf("Task data: %s", string(data))

	task := &mesos.TaskInfo{
		Name:     proto.String(taskName),
		TaskId:   taskId,
		SlaveId:  offer.GetSlaveId(),
		Executor: s.createExecutor(offer.GetHostname()),
		Resources: []*mesos.Resource{
			util.NewScalarResource("cpus", Config.Cpus),
			util.NewScalarResource("mem", Config.Mem),
		},
		Data:   data,
		Labels: utils.StringToLabels(s.labels),
	}

	s.cluster.Add(offer.GetHostname(), task)

	driver.LaunchTasks([]*mesos.OfferID{offer.GetId()}, []*mesos.TaskInfo{task}, &mesos.Filters{RefuseSeconds: proto.Float64(1)})
}

func (s *Scheduler) createExecutor(hostname string) *mesos.ExecutorInfo {
	id := fmt.Sprintf("statsd-kafka-%s", hostname)

	uris := []*mesos.CommandInfo_URI{
		&mesos.CommandInfo_URI{
			Value:      proto.String(fmt.Sprintf("%s/resource/%s", Config.Api, Config.Executor)),
			Executable: proto.Bool(true),
		},
	}

	if Config.ProducerProperties != "" {
		uris = append(uris, &mesos.CommandInfo_URI{
			Value: proto.String(fmt.Sprintf("%s/resource/%s", Config.Api, Config.ProducerProperties)),
		})
	}

	return &mesos.ExecutorInfo{
		ExecutorId: util.NewExecutorID(id),
		Name:       proto.String(id),
		Command: &mesos.CommandInfo{
			Value: proto.String(fmt.Sprintf("./%s --log.level %s --host %s", Config.Executor, Config.LogLevel, hostname)),
			Uris:  uris,
		},
	}
}

func (s *Scheduler) hostnameFromTaskId(taskId string) string {
	tokens := strings.SplitN(taskId, "-", 3)
	hostname := tokens[len(tokens)-1]
	hostname = hostname[:len(hostname)-37] //strip uuid part
	Logger.Debugf("Hostname extracted from %s is %s", taskId, hostname)
	return hostname
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

func (s *Scheduler) listenAddr() string {
	address := Config.Api
	if strings.HasPrefix(address, "http://") {
		address = address[len("http://"):]
	}

	colonIndex := strings.LastIndex(address, ":")
	if colonIndex != -1 {
		address = "0.0.0.0" + address[colonIndex:]
	}

	return address
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
