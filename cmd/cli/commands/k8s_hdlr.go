// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This file handles CLI commands that pertain to AIS buckets.
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"
	"os/exec"

	"github.com/NVIDIA/aistore/api"
	"github.com/urfave/cli"
)

var (
	k8sCmdsFlags = map[string][]cli.Flag{
		subcmdK8sSvc:     {},
		subcmdK8sCluster: {},
	}

	k8sCmd = cli.Command{
		Name:  subcmdK8s,
		Usage: "show kubernetes pods and services",
		Subcommands: []cli.Command{
			{
				Name:   subcmdK8sSvc,
				Usage:  "show kubernetes services",
				Flags:  k8sCmdsFlags[subcmdK8sSvc],
				Action: k8sShowSvcHandler,
			},
			{
				Name:      subcmdK8sCluster,
				Usage:     "show AIS cluster",
				Flags:     k8sCmdsFlags[subcmdK8sCluster],
				ArgsUsage: optionalDaemonIDArgument,
				Action:    k8sShowClusterHandler,
				BashComplete: func(c *cli.Context) {
					if c.NArg() != 0 {
						return
					}
					suggestDaemon(completeAllDaemons)
				},
			},
		},
	}

	// kubectl command lines
	cmdPodList  = []string{"get", "pods"}
	cmdSvcList  = []string{"get", "svc", "-n", "ais"}
	cmdNodeInfo = []string{"get", "pods", "-n", "ais", "-o=wide"}
)

func k8sShowSvcHandler(c *cli.Context) (err error) {
	output, err := exec.Command("kubectl", cmdSvcList...).CombinedOutput()
	if err != nil {
		return err
	}
	fmt.Fprint(c.App.Writer, string(output))
	return nil
}

func k8sShowClusterHandler(c *cli.Context) error {
	if c.NArg() == 0 {
		return k8sShowEntireCluster(c)
	}
	return k8sShowSingleDaemon(c)
}

func k8sShowEntireCluster(c *cli.Context) (err error) {
	output, err := exec.Command(subcmdK8s, cmdPodList...).CombinedOutput()
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(c.App.Writer, string(output))
	return err
}

func k8sShowSingleDaemon(c *cli.Context) (err error) {
	smap, err := api.GetClusterMap(defaultAPIParams)
	if err != nil {
		return err
	}
	daemonID := c.Args().First()
	if node := smap.GetNode(daemonID); node == nil {
		return fmt.Errorf("%s does not exist in the cluster (see 'ais show cluster')", daemonID)
	}
	cmdLine := make([]string, 0, len(cmdNodeInfo)+1)
	cmdLine = append(cmdLine, cmdNodeInfo...)
	cmdLine = append(cmdLine, "--selector=ais-daemon-id="+daemonID)
	output, err := exec.Command(subcmdK8s, cmdLine...).CombinedOutput()
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(c.App.Writer, string(output))
	return err
}
