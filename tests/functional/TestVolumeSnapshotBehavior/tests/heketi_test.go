// +build func

//
// Copyright (c) 2015 The heketi Authors
//
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
//
package functional

import (
	"fmt"
	"testing"

	"github.com/heketi/heketi/apps/glusterfs"
	client "github.com/heketi/heketi/client/api/go-client"
	"github.com/heketi/tests"
	"github.com/heketi/utils"
	"github.com/heketi/utils/ssh"
)

// These are the settings for the vagrant file
const (

	// The heketi server must be running on the host
	heketiUrl = "http://127.0.0.1:8080"

	// VM Information
	DISKS    = 8
	NODES    = 4
	ZONES    = 4
	CLUSTERS = 1
)

var (
	// Heketi client
	heketi = client.NewClient(heketiUrl, "admin", "adminkey")
	logger = utils.NewLogger("[test]", utils.LEVEL_DEBUG)
)

func getdisks() []string {

	diskletters := make([]string, DISKS)
	for index, i := 0, []byte("b")[0]; index < DISKS; index, i = index+1, i+1 {
		diskletters[index] = "/dev/vd" + string(i)
	}

	return diskletters
}

func getnodes() []string {
	nodelist := make([]string, NODES)

	for index, ip := 0, 100; index < NODES; index, ip = index+1, ip+1 {
		nodelist[index] = "192.168.10." + fmt.Sprintf("%v", ip)
	}

	return nodelist
}

func setupCluster(t *testing.T) {
	tests.Assert(t, heketi != nil)

	nodespercluster := NODES / CLUSTERS
	nodes := getnodes()
	sg := utils.NewStatusGroup()
	for cluster := 0; cluster < CLUSTERS; cluster++ {
		sg.Add(1)
		go func(nodes_in_cluster []string) {
			defer sg.Done()
			// Create a cluster
			cluster, err := heketi.ClusterCreate()
			if err != nil {
				logger.Err(err)
				sg.Err(err)
				return
			}

			// Add nodes sequentially due to probes
			for index, hostname := range nodes_in_cluster {
				nodeReq := &glusterfs.NodeAddRequest{}
				nodeReq.ClusterId = cluster.Id
				nodeReq.Hostnames.Manage = []string{hostname}
				nodeReq.Hostnames.Storage = []string{hostname}
				nodeReq.Zone = index%ZONES + 1

				node, err := heketi.NodeAdd(nodeReq)
				if err != nil {
					logger.Err(err)
					sg.Err(err)
					return
				}

				// Add devices all concurrently
				for _, disk := range getdisks() {
					sg.Add(1)
					go func(d string) {
						defer sg.Done()

						driveReq := &glusterfs.DeviceAddRequest{}
						driveReq.Name = d
						driveReq.NodeId = node.Id

						err := heketi.DeviceAdd(driveReq)
						if err != nil {
							logger.Err(err)
							sg.Err(err)
						}
					}(disk)
				}
			}
		}(nodes[cluster*nodespercluster : (cluster+1)*nodespercluster])
	}

	// Wait here for results
	err := sg.Result()
	tests.Assert(t, err == nil)

}

func teardownCluster(t *testing.T) {
	clusters, err := heketi.ClusterList()
	tests.Assert(t, err == nil)

	sg := utils.NewStatusGroup()
	for _, cluster := range clusters.Clusters {

		sg.Add(1)
		go func(clusterId string) {
			defer sg.Done()

			clusterInfo, err := heketi.ClusterInfo(clusterId)
			if err != nil {
				logger.Err(err)
				sg.Err(err)
				return
			}

			// Delete volumes in this cluster
			for _, volume := range clusterInfo.Volumes {
				err := heketi.VolumeDelete(volume)
				if err != nil {
					logger.Err(err)
					sg.Err(err)
					return
				}
			}

			// Delete all devices in the cluster concurrently
			deviceSg := utils.NewStatusGroup()
			for _, node := range clusterInfo.Nodes {

				// Get node information
				nodeInfo, err := heketi.NodeInfo(node)
				if err != nil {
					logger.Err(err)
					deviceSg.Err(err)
					return
				}

				// Delete each device
				for _, device := range nodeInfo.DevicesInfo {
					deviceSg.Add(1)
					go func(id string) {
						defer deviceSg.Done()

						err := heketi.DeviceDelete(id)
						if err != nil {
							logger.Err(err)
							deviceSg.Err(err)
							return
						}

					}(device.Id)
				}
			}
			err = deviceSg.Result()
			if err != nil {
				logger.Err(err)
				sg.Err(err)
				return
			}

			// Delete nodes
			for _, node := range clusterInfo.Nodes {
				err = heketi.NodeDelete(node)
				if err != nil {
					logger.Err(err)
					sg.Err(err)
					return
				}
			}

			// Delete cluster
			err = heketi.ClusterDelete(clusterId)
			if err != nil {
				logger.Err(err)
				sg.Err(err)
				return
			}

		}(cluster)

	}

	err = sg.Result()
	tests.Assert(t, err == nil)
}

func TestConnection(t *testing.T) {
	err := heketi.Hello()
	tests.Assert(t, err == nil)
}

func TestHeketiVolumeSnapshotBehavior(t *testing.T) {

	// Setup the VM storage topology
	teardownCluster(t)
	setupCluster(t)
	defer teardownCluster(t)

	// Create a volume
	volReq := &glusterfs.VolumeCreateRequest{}
	volReq.Size = 1024
	volReq.Durability.Type = client.VOLUME_CREATE_DURABILITY_TYPE_REPLICATE
	volReq.Durability.Replicate.Replica = 3
	volReq.Snapshot.Enable = true
	volReq.Snapshot.Factor = 1.5

	volInfo, err := heketi.VolumeCreate(volReq)
	tests.Assert(t, err == nil)

	// SSH into system and execute gluster command to create a snapshot
	exec := ssh.NewSshExecWithKeyFile(logger, "vagrant", "../config/insecure_private_key")
	cmd := []string{
		fmt.Sprintf("sudo gluster --mode=script snapshot create mysnap %v no-timestamp", volInfo.Name),
		"sudo gluster --mode=script snapshot activate mysnap",
	}
	_, err = exec.ConnectAndExec("192.168.10.100:22", cmd, 10)
	tests.Assert(t, err == nil, err)

	// Try to delete the volume
	err = heketi.VolumeDelete(volInfo.Id)
	tests.Assert(t, err != nil, err)

	// Now clean up the snapshot
	cmd = []string{
		"sudo gluster --mode=script snapshot deactivate mysnap",
		"sudo gluster --mode=script snapshot delete mysnap",
	}
	_, err = exec.ConnectAndExec("192.168.10.100:22", cmd, 10)
	tests.Assert(t, err == nil, err)

	// Try to delete the volume
	err = heketi.VolumeDelete(volInfo.Id)
	tests.Assert(t, err == nil, err)
}
