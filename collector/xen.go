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

// +build yesxen

package collector

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"context"

	xenAPI "github.com/amfranz/go-xen-api-client"
	"github.com/prometheus/client_golang/prometheus"
)

const xapiSocket = "/var/xapi/xapi"

type xenCollector struct {
	metrics        []prometheus.Gauge
	metricsStaging []prometheus.Gauge
}

func init() {
	Factories["xen"] = NewXenCollector
}

// Take a prometheus registry and return a new Collector exposing xen data.
func NewXenCollector() (Collector, error) {
	return &xenCollector{}, nil
}

func (c *xenCollector) Update(ch chan<- prometheus.Metric) (err error) {

	var boolToFloat = func(inBool bool) float64 {
		if inBool {
			return 1
		}
		return 0
	}

	var newMetric = func(name string, help string,
		labels prometheus.Labels, value float64) prometheus.Gauge {

		metric := prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   Namespace,
			Name:        "xen_" + name,
			Help:        help,
			ConstLabels: labels,
		})
		return metric
	}

	var myHostRef xenAPI.HostRef
	var iAmMaster bool
	myHostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("couldn't get my own hostname: %s", err)
	}

	xenTransport := &http.Transport{
		DialContext: func(ctx context.Context, network,
			addr string) (net.Conn, error) {
			return net.Dial("unix", xapiSocket)
		},
	}

	xenClient, err := xenAPI.NewClient("http://localhost/", xenTransport)
	if err != nil {
		return fmt.Errorf("couldn't create xapi client: %s", err.Error())
	}

	sessionID, err := xenClient.Session.LoginWithPassword(
		"root", "", "1.0", "node_exporter")
	if err != nil {
		return fmt.Errorf("couldn't login to xapi socket: %s", err.Error())
	}

	poolRecs, err := xenClient.Pool.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("error getting pool records: %s", err.Error())
	}

	hostRecs, err := xenClient.Host.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("error getting host records: %s\n", err.Error())
	}

	vmRecs, err := xenClient.VM.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("error getting vm records: %s\n", err.Error())
	}

	vmMetricsRecs, err := xenClient.VMMetrics.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("error getting vm metrics records: %s\n", err.Error())
	}

	hostMetricsRecs, err := xenClient.HostMetrics.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("Error getting host metrics records: %s\n", err.Error())
	}

	srRecs, err := xenClient.SR.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("error getting sr records: %s", err)
	}

	pbdRecs, err := xenClient.PBD.GetAllRecords(sessionID)
	if err != nil {
		return fmt.Errorf("error getting pbd records: %s", err)
	}

	for host, hostRec := range hostRecs {

		hostMetricsRec := hostMetricsRecs[hostRec.Metrics]

		hostLabels := prometheus.Labels{"host": hostRec.Hostname}

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
		cpuCountMetric := newMetric("cpu_count",
			"number of physical cpus on the host",
			hostLabels, cpuCount)
		c.metricsStaging = append(c.metricsStaging, cpuCountMetric)

		// set cpu_allocation metric for the host
		cpuPctAllocatedMetric := newMetric("cpu_pct_allocated",
			"percent of vCPUs over physical CPUs",
			hostLabels, float64(vCPUCount)*100/cpuCount)
		c.metricsStaging = append(c.metricsStaging, cpuPctAllocatedMetric)

		// set memory_total metric for host
		memoryTotalMetric := newMetric("memory_total",
			"total host memory (bytes)", hostLabels,
			float64(hostMetricsRecs[hostRec.Metrics].MemoryTotal))
		c.metricsStaging = append(c.metricsStaging, memoryTotalMetric)

		// set memory_free metric for host
		memoryFreeMetric := newMetric("memory_free",
			"free host memory (bytes)", hostLabels,
			float64(hostMetricsRecs[hostRec.Metrics].MemoryFree))
		c.metricsStaging = append(c.metricsStaging, memoryFreeMetric)

		// set memory_allocation metric for host
		memory_used := hostMetricsRec.MemoryTotal - hostMetricsRec.MemoryFree
		memoryPctAllocatedMetric := newMetric("memory_pct_allocated",
			"percent of memory_total less memory_free over memory_total",
			hostLabels,
			float64(memory_used)*100/float64(hostMetricsRec.MemoryTotal))
		c.metricsStaging = append(c.metricsStaging, memoryPctAllocatedMetric)

		// set resident_vcpu_count metric for host
		residentVCPUCountMetric := newMetric("resident_vcpu_count",
			"count of vCPUs on VMs running on the host", hostLabels,
			float64(vCPUCount))
		c.metricsStaging = append(c.metricsStaging, residentVCPUCountMetric)

		// set resident_vm_count metric for host
		residentVMCountMetric := newMetric("resident_vm_count",
			"count of VMs running on the host", hostLabels,
			float64(vmCount))
		c.metricsStaging = append(c.metricsStaging, residentVMCountMetric)

	}

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

		poolLabels := prometheus.Labels{"pool": poolRec.NameLabel}

		// set ha_allow_overcommit metric for pool
		haAllowOvercommitMetric := newMetric("ha_allow_overcommit",
			"if set to false then operations which would cause the pool to become "+
				"overcommitted will be blocked", poolLabels,
			boolToFloat(poolRec.HaAllowOvercommit))
		c.metricsStaging = append(c.metricsStaging, haAllowOvercommitMetric)

		// set ha_enabled metric for pool
		haEnabledMetric := newMetric("ha_enabled",
			"true if HA is enabled on the pool", poolLabels,
			boolToFloat(poolRec.HaEnabled))
		c.metricsStaging = append(c.metricsStaging, haEnabledMetric)

		// set ha_host_failures_to_tolerate metric for pool
		haHostFailuresToTolerateMetric := newMetric(
			"ha_host_failures_to_tolerate",
			"number of host failures to tolerate before the "+
				"pool is declared to be overcommitted", poolLabels,
			float64(poolRec.HaHostFailuresToTolerate))
		c.metricsStaging = append(c.metricsStaging, haHostFailuresToTolerateMetric)

		// set the ha_overcommitted metric for pool
		haOvercommittedMetric := newMetric("ha_overcommitted",
			"true if the pool is considered to be overcommitted", poolLabels,
			boolToFloat(poolRec.HaOvercommitted))
		c.metricsStaging = append(c.metricsStaging, haOvercommittedMetric)

		// set the wlb_enabled metric for the pool
		wlbEnabledMetric := newMetric("wlb_enabled",
			"true if workload balancing is enabled on the pool", poolLabels,
			boolToFloat(poolRec.WlbEnabled))
		c.metricsStaging = append(c.metricsStaging, wlbEnabledMetric)
	}

	for srRef, srRec := range srRecs {
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
		defaultStorageMetric := newMetric("default_storage",
			"true if SR is a default SR for VDIs", srLabels,
			boolToFloat(defaultSR))
		c.metricsStaging = append(c.metricsStaging, defaultStorageMetric)

		// set the physical_size metric for the sr
		physicalSizeMetric := newMetric("physical_size",
			"total physicalk size of the repository (in bytes)", srLabels,
			float64(srRec.PhysicalSize))
		c.metricsStaging = append(c.metricsStaging, physicalSizeMetric)

		// set the physical_utilisation metric for the sr
		physicalUtilisationMetric := newMetric("physical_utilisation",
			"physical space currently utilised on this storage repository (bytes)",
			srLabels, float64(srRec.PhysicalUtilisation))
		c.metricsStaging = append(c.metricsStaging, physicalUtilisationMetric)

		// set the physical_pct_allocated metric for the sr
		physicalPctAllocated := float64(0)
		if srRec.PhysicalSize > 0 {
			physicalPctAllocated = float64(srRec.PhysicalUtilisation) * 100 /
				float64(srRec.PhysicalSize)
		}
		physicalPctAllocatedMetric := newMetric("physical_pct_allocated",
			"percent of physical_utilisation over physical_size", srLabels,
			physicalPctAllocated)
		c.metricsStaging = append(c.metricsStaging, physicalPctAllocatedMetric)

		// set the virtual_allocation metric for the sr
		virtualAllocationMetric := newMetric("virtual_allocation",
			"sum of virtualk_sizes of all VDIs in the SR (bytes)",
			srLabels, float64(srRec.VirtualAllocation))
		c.metricsStaging = append(c.metricsStaging, virtualAllocationMetric)

	}

	c.metrics = c.metricsStaging
	for _, metric := range c.metrics {
		metric.Collect(ch)
	}
	return err
}
