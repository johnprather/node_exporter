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

type xenCollector struct {
	metrics     []prometheus.Gauge
	boolToFloat map[bool]float64
}

func init() {
	Factories["xapi"] = NewXenCollector
}

// Take a prometheus registry and return a new Collector exposing xen data.
func NewXenCollector() (Collector, error) {
	c := &xenCollector{}

	// this comes in handy for quickly throwing together boolean metrics
	c.boolToFloat = map[bool]float64{true: 1, false: 0}
	return c, nil
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

// genXAPIMetrics pulls data from xapi and populates newMetrics
func (c *xenCollector) genXAPIMetrics() ([]prometheus.Gauge, error) {

	// init an empty array of metrics
	newMetrics := make([]prometheus.Gauge, 0)

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

func (c *xenCollector) Update(ch chan<- prometheus.Metric) (err error) {
	// init metrics
	var metrics []prometheus.Gauge

	// channels for catching results from the metrics generation go routine
	genErrCh := make(chan error)
	genMetricsCh := make(chan []prometheus.Gauge)

	// launch generation in own routine, return results through genErrCh
	go func(c *xenCollector, genErrCh chan error,
		genMetricsCh chan []prometheus.Gauge) {

		// attempt to generate xapi stats
		genMetrics, genErr := c.genXAPIMetrics()
		if genErr != nil {
			// error occurred, send it through the error chan
			genErrCh <- genErr
		} else {
			// no error, send metric array through the metrics chan
			genMetricsCh <- genMetrics
		}

	}(c, genErrCh, genMetricsCh)

	// init a failure bool to false
	xapiFailure := false

	// wait for either results or a timeout
	select {

	// handle error passed from the metrics generation go routine
	case xenErr := <-genErrCh:
		log.Errorln(xenErr)
		xapiFailure = true

	// handle metric array from metrics generation go routine
	case xenMetrics := <-genMetricsCh:
		metrics = xenMetrics

	// handle timeout waiting for metrics generation
	case <-time.After(time.Duration(xapiTimeout) * time.Second):
		log.Errorf("xapi stats generation timeout after %d seconds", xapiTimeout)
		xapiFailure = true

	}

	// set the failure metric for the xapi collector as a whole
	xapiFailureMetric := c.newMetric("failure",
		"boolean indicates problem with xapi interface",
		nil, c.boolToFloat[xapiFailure])
	metrics = append(metrics, xapiFailureMetric)

	// report metrics!
	for _, metric := range metrics {
		metric.Collect(ch)
	}
	return err
}
