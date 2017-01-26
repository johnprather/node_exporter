// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build xapi

package collector

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"context"

	"github.com/prometheus/common/log"

	xenAPI "github.com/johnprather/go-xen-api-client"
	"github.com/prometheus/client_golang/prometheus"
)

const xapiSocket = "/var/xapi/xapi"
const xapiPrefix = "xapi"
const xapiTimeout = 5
const xapiInterval = 60

type xenCollector struct {
	metrics               XenMetricSet
	metricsRead           chan chan XenMetricSet
	metricsWrite          chan XenMetricSet
	metricsLastUpdate     int64
	metricsLastUpdateRead chan chan int64
	launched              bool
	launchedRead          chan chan bool
	launchedWrite         chan chan bool
	boolToFloat           map[bool]float64
}

type XenMetricSet []prometheus.Gauge

func init() {
	Factories["xapi"] = NewXenCollector
}

// Take a prometheus registry and return a new Collector exposing xen data.
func NewXenCollector() (Collector, error) {
	c := &xenCollector{}

	// this comes in handy for quickly throwing together boolean metrics
	c.boolToFloat = map[bool]float64{true: 1, false: 0}

	// setup metrics read/writes
	c.metricsRead = make(chan chan XenMetricSet)
	c.metricsWrite = make(chan XenMetricSet)
	c.metricsLastUpdateRead = make(chan chan int64)
	c.launchedRead = make(chan chan bool)
	c.launchedWrite = make(chan chan bool)
	go c.listenRW()
	return c, nil
}

// listenRW is a routine that handles read/write of shared data
func (c *xenCollector) listenRW() {
	for {
		select {
		case ch := <-c.metricsRead:
			ch <- c.metrics.dup()
		case metrics := <-c.metricsWrite:
			c.metrics = metrics.dup()
			c.metricsLastUpdate = time.Now().Unix()
		case ch := <-c.launchedRead:
			ch <- c.launched
		case ch := <-c.launchedWrite:
			changed := false
			if !c.launched {
				changed = true
			}
			c.launched = true
			ch <- changed
		case ch := <-c.metricsLastUpdateRead:
			ch <- c.metricsLastUpdate
		}
	}
}

// setMetrics is a thread safe write for c.metrics
func (c *xenCollector) setMetrics(metrics XenMetricSet) {
	c.metricsWrite <- metrics
}

// getMetrics is a thread-safe read for c.metrics
func (c *xenCollector) getMetrics() XenMetricSet {
	ch := make(chan XenMetricSet)
	c.metricsRead <- ch
	metrics := <-ch
	return metrics
}

// getMetricsLastUpdate is a thread-safe read for c.metricsLastUpdate
func (c *xenCollector) getMetricsLastUpdate() int64 {
	ch := make(chan int64)
	c.metricsLastUpdateRead <- ch
	metricsLastUpdate := <-ch
	return metricsLastUpdate
}

// setLaunched is a thread-safe write for c.launched
func (c *xenCollector) setLaunched() bool {
	ch := make(chan bool)
	c.launchedWrite <- ch
	changed := <-ch
	return changed
}

// getLaunched is a thread-safe read for c.launched
func (c *xenCollector) getLaunched() bool {
	ch := make(chan bool)
	c.launchedRead <- ch
	launched := <-ch
	return launched
}

// newMetric instantiates a prometheus Gauge object
func (c *xenCollector) newMetric(name string, help string,
	labels prometheus.Labels, value float64) prometheus.Gauge {

	metric := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   Namespace,
		Name:        xapiPrefix + "_" + name,
		Help:        help,
		ConstLabels: labels,
	})
	metric.Set(value)
	return metric
}

// dup creates a new XenMetricSet which is a copy of s (slices are pointers)
func (s XenMetricSet) dup() XenMetricSet {
	var set XenMetricSet
	set = append(set, s...)
	return set
}

// genXAPIMetrics pulls data from xapi and populates newMetrics
func (c *xenCollector) genXAPIMetrics() ([]prometheus.Gauge, error) {
	var newMetrics XenMetricSet
	var myHostRef xenAPI.HostRef
	var iAmMaster bool

	myHostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("couldn't get my own hostname: %s", err)
	}

	xenTransport := &http.Transport{
		DialContext: func(ctx context.Context, network,
			addr string) (net.Conn, error) {
			return net.Dial("unix", xapiSocket)
		},
	}

	xenClient, err := xenAPI.NewClient("http://localhost/", xenTransport)
	if err != nil {
		return nil, fmt.Errorf("couldn't create xapi client: %s", err.Error())
	}

	defer xenClient.Close()

	sessionID, err := xenClient.Session.LoginWithPassword(
		"root", "", "1.0", "node_exporter")
	if err != nil {
		return nil, fmt.Errorf("couldn't login to xapi socket: %s", err.Error())
	}

	poolRecs, err := xenClient.Pool.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error getting pool records: %s", err.Error())
	}

	hostRecs, err := xenClient.Host.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error getting host records: %s\n", err.Error())
	}

	vmRecs, err := xenClient.VM.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error getting vm records: %s\n", err.Error())
	}

	vmMetricsRecs, err := xenClient.VMMetrics.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error getting vm metrics records: %s\n", err.Error())
	}

	hostMetricsRecs, err := xenClient.HostMetrics.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("Error getting host metrics records: %s\n", err.Error())
	}

	srRecs, err := xenClient.SR.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error getting sr records: %s", err)
	}

	pbdRecs, err := xenClient.PBD.GetAllRecords(sessionID)
	if err != nil {
		return nil, fmt.Errorf("error getting pbd records: %s", err)
	}

	for host, hostRec := range hostRecs {

		hostMetricsRec := hostMetricsRecs[hostRec.Metrics]

		// any labels suitable for including in host-specific metrics
		hostLabels := prometheus.Labels{}

		// see if we can figure out our own host opaqueref
		if hostRec.Hostname == myHostname {
			// this host is me! report remember our HostRef for later
			myHostRef = host
		} else {
			// this host is not me, don't report its metrics
			continue
		}

		// tally vcpus and vms
		vCPUCount := 0
		vmCount := 0
		for _, vmRef := range hostRec.ResidentVMs {
			if vmRec, ok := vmRecs[vmRef]; ok && !vmRec.IsControlDomain {
				vmCount++
				if vmMetricsRec, ok := vmMetricsRecs[vmRec.Metrics]; ok {
					vCPUCount += vmMetricsRec.VCPUsNumber
				}
			}
		}

		cpuCount, _ := strconv.ParseFloat(hostRec.CPUInfo["cpu_count"], 64)

		// set cpu_count metric for the host
		cpuCountMetric := c.newMetric("cpu_count",
			"number of physical cpus on the host",
			hostLabels, cpuCount)
		newMetrics = append(newMetrics, cpuCountMetric)

		// set cpu_allocation metric for the host
		cpuPctAllocatedMetric := c.newMetric("cpu_pct_allocated",
			"percent of vCPUs over physical CPUs",
			hostLabels, float64(vCPUCount)*100/cpuCount)
		newMetrics = append(newMetrics, cpuPctAllocatedMetric)

		// set memory_total metric for host
		memoryTotalMetric := c.newMetric("memory_total",
			"total host memory (bytes)", hostLabels,
			float64(hostMetricsRecs[hostRec.Metrics].MemoryTotal))
		newMetrics = append(newMetrics, memoryTotalMetric)

		// set memory_free metric for host
		memoryFreeMetric := c.newMetric("memory_free",
			"free host memory (bytes)", hostLabels,
			float64(hostMetricsRecs[hostRec.Metrics].MemoryFree))
		newMetrics = append(newMetrics, memoryFreeMetric)

		// set memory_allocation metric for host
		memory_used := hostMetricsRec.MemoryTotal - hostMetricsRec.MemoryFree
		memoryPctAllocatedMetric := c.newMetric("memory_pct_allocated",
			"percent of memory_total less memory_free over memory_total",
			hostLabels,
			float64(memory_used)*100/float64(hostMetricsRec.MemoryTotal))
		newMetrics = append(newMetrics, memoryPctAllocatedMetric)

		// set resident_vcpu_count metric for host
		residentVCPUCountMetric := c.newMetric("resident_vcpu_count",
			"count of vCPUs on VMs running on the host", hostLabels,
			float64(vCPUCount))
		newMetrics = append(newMetrics, residentVCPUCountMetric)

		// set resident_vm_count metric for host
		residentVMCountMetric := c.newMetric("resident_vm_count",
			"count of VMs running on the host", hostLabels,
			float64(vmCount))
		newMetrics = append(newMetrics, residentVMCountMetric)

	}

	// maintain list of default pool SRs for default metric later in SR loop
	var defaultSRList []xenAPI.SRRef

	for _, poolRec := range poolRecs {

		defaultSRList = append(defaultSRList, poolRec.DefaultSR)

		// are we this pool's master?
		if poolRec.Master == myHostRef {
			// we are the master, remember this for later
			iAmMaster = true
		} else {
			// we are not the master, skip metrics for this pool
			continue
		}

		// prom lables suitable for a pool
		poolLabels := prometheus.Labels{"pool": poolRec.NameLabel}

		// set ha_allow_overcommit metric for pool
		haAllowOvercommitMetric := c.newMetric("ha_allow_overcommit",
			"if set to false then operations which would cause the pool to become "+
				"overcommitted will be blocked", poolLabels,
			c.boolToFloat[poolRec.HaAllowOvercommit])
		newMetrics = append(newMetrics, haAllowOvercommitMetric)

		// set ha_enabled metric for pool
		haEnabledMetric := c.newMetric("ha_enabled",
			"true if HA is enabled on the pool", poolLabels,
			c.boolToFloat[poolRec.HaEnabled])
		newMetrics = append(newMetrics, haEnabledMetric)

		// set ha_host_failures_to_tolerate metric for pool
		haHostFailuresToTolerateMetric := c.newMetric(
			"ha_host_failures_to_tolerate",
			"number of host failures to tolerate before the "+
				"pool is declared to be overcommitted", poolLabels,
			float64(poolRec.HaHostFailuresToTolerate))
		newMetrics = append(newMetrics, haHostFailuresToTolerateMetric)

		// set the ha_overcommitted metric for pool
		haOvercommittedMetric := c.newMetric("ha_overcommitted",
			"true if the pool is considered to be overcommitted", poolLabels,
			c.boolToFloat[poolRec.HaOvercommitted])
		newMetrics = append(newMetrics, haOvercommittedMetric)

		// set the wlb_enabled metric for the pool
		wlbEnabledMetric := c.newMetric("wlb_enabled",
			"true if workload balancing is enabled on the pool", poolLabels,
			c.boolToFloat[poolRec.WlbEnabled])
		newMetrics = append(newMetrics, wlbEnabledMetric)
	}

	for srRef, srRec := range srRecs {

		// metric labels suitable for a SR
		srLabels := prometheus.Labels{
			"uuid":       srRec.UUID,
			"type":       srRec.Type,
			"name_label": srRec.NameLabel,
		}

		if srRec.Shared {
			// this is a shared SR, if we are not the master, do not report metrics
			if !iAmMaster {
				continue
			}
		} else {
			// this is a non-shared SR, if not on this host do not report metrics
			isMySR := false
			for _, pbd := range srRec.PBDs {
				if pbdRecs[pbd].Host == myHostRef {
					isMySR = true
					break
				}
			}
			if !isMySR {
				continue
			}
		}

		defaultSR := false
		for _, defSR := range defaultSRList {
			if defSR == srRef {
				defaultSR = true
			}
		}

		// set the default_storage metric for the sr
		defaultStorageMetric := c.newMetric("default_storage",
			"true if SR is a default SR for VDIs", srLabels,
			c.boolToFloat[defaultSR])
		newMetrics = append(newMetrics, defaultStorageMetric)

		// set the physical_size metric for the sr
		physicalSizeMetric := c.newMetric("physical_size",
			"total physicalk size of the repository (in bytes)", srLabels,
			float64(srRec.PhysicalSize))
		newMetrics = append(newMetrics, physicalSizeMetric)

		// set the physical_utilisation metric for the sr
		physicalUtilisationMetric := c.newMetric("physical_utilisation",
			"physical space currently utilised on this storage repository (bytes)",
			srLabels, float64(srRec.PhysicalUtilisation))
		newMetrics = append(newMetrics, physicalUtilisationMetric)

		// set the physical_pct_allocated metric for the sr
		physicalPctAllocated := float64(0)
		if srRec.PhysicalSize > 0 {
			physicalPctAllocated = float64(srRec.PhysicalUtilisation) * 100 /
				float64(srRec.PhysicalSize)
		}
		physicalPctAllocatedMetric := c.newMetric("physical_pct_allocated",
			"percent of physical_utilisation over physical_size", srLabels,
			physicalPctAllocated)
		newMetrics = append(newMetrics, physicalPctAllocatedMetric)

		// set the virtual_allocation metric for the sr
		virtualAllocationMetric := c.newMetric("virtual_allocation",
			"sum of virtualk_sizes of all VDIs in the SR (bytes)",
			srLabels, float64(srRec.VirtualAllocation))
		newMetrics = append(newMetrics, virtualAllocationMetric)

	}

	return newMetrics, nil
}

// gatherLoop is intended to run as a go routine for handling metrics population
func (c *xenCollector) gatherLoop() {
	log.Debugln("starting gatherLoop")
	defer log.Debugln("returning from gatherLoop")
	for {
		xapiFailure := false

		// attempt to generate xapi metrics
		genMetrics, genErr := c.genXAPIMetrics()
		if genErr != nil {
			log.Errorln(genErr)
			xapiFailure = true
		}

		// set xapi failure metric appropriately
		xapiFailureMetric := c.newMetric("failure",
			"boolean indicates problem with xapi interface",
			nil, c.boolToFloat[xapiFailure])
		genMetrics = append(genMetrics, xapiFailureMetric)

		// save our newly generated metrics to the collector's shared metrics object
		c.setMetrics(genMetrics)

		// wait a bit before doing it all over again
		time.Sleep(time.Duration(xapiInterval) * time.Second)
	}
}

func (c *xenCollector) Update(ch chan<- prometheus.Metric) (err error) {

	// if setLaunched results in a change, launch gathering thread
	if c.setLaunched() {
		go c.gatherLoop()
	}

	// if gatherloop was just launched, metrics may be empty
	// wait up to 5 secs @ 100ms intervals for metrics to populate
	var metrics = c.getMetrics()
	if len(metrics) == 0 {
		startTime := time.Now().Unix()
		for len(metrics) == 0 {
			if time.Now().Unix()-startTime > 5 {
				break
			}
			time.Sleep(time.Duration(100) * time.Millisecond)
			metrics = c.getMetrics()
		}
	}

	// if we have last update info, add it to metrics so staleness is detectable
	lastUpdate := c.getMetricsLastUpdate()

	if lastUpdate != 0 {
		ageMetric := c.newMetric("xapi_metrics_age",
			"age in seconds of current xapi metrics", nil,
			float64(time.Now().Unix()-lastUpdate))
		metrics = append(metrics, ageMetric)
	}

	// report metrics!
	for _, metric := range metrics {
		metric.Collect(ch)
	}
	return err
}
