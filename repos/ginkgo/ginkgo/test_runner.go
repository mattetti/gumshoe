package main

import (
	"bytes"
	"fmt"
	"github.com/pivotal/gumshoe/repos/ginkgo/config"
	"github.com/pivotal/gumshoe/repos/ginkgo/ginkgo/aggregator"
	"github.com/pivotal/gumshoe/repos/ginkgo/ginkgo/testsuite"
	"github.com/pivotal/gumshoe/repos/ginkgo/remote"
	"github.com/pivotal/gumshoe/repos/ginkgo/stenographer"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"time"
)

type testRunner struct {
	numCPU           int
	parallelStream   bool
	runMagicI        bool
	race             bool
	cover            bool
	executedCommands []*exec.Cmd
	reports          []*bytes.Buffer
}

func newTestRunner(numCPU int, parallelStream bool, runMagicI bool, race bool, cover bool) *testRunner {
	return &testRunner{
		numCPU:           numCPU,
		parallelStream:   parallelStream,
		runMagicI:        runMagicI,
		race:             race,
		cover:            cover,
		executedCommands: []*exec.Cmd{},
		reports:          []*bytes.Buffer{},
	}
}

func (t *testRunner) run(suites []*testsuite.TestSuite) bool {
	t.registerSignalHandler()

	for _, suite := range suites {
		if !t.runSuite(suite) {
			return false
		}
	}

	return true
}

func (t *testRunner) runSuite(suite *testsuite.TestSuite) bool {
	if t.runMagicI {
		err := t.runGoI(suite)
		if err != nil {
			return false
		}
	}

	if suite.IsGinkgo {
		if t.numCPU > 1 {
			if t.parallelStream {
				return t.runAndStreamParallelGinkgoSuite(suite)
			} else {
				return t.runParallelGinkgoSuite(suite)
			}
		} else {
			return t.runSerialGinkgoSuite(suite)
		}
	} else {
		return t.runGoTestSuite(suite)
	}
}

func (t *testRunner) runGoI(suite *testsuite.TestSuite) error {
	args := []string{"test", "-i"}
	if t.race {
		args = append(args, "-race")
	}
	args = append(args, suite.Path)
	cmd := exec.Command("go", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("go test -i %s failed with:\n\n%s", suite.Path, output)
	}

	return err
}

func (t *testRunner) runParallelGinkgoSuite(suite *testsuite.TestSuite) bool {
	completions := make(chan bool)
	for cpu := 0; cpu < t.numCPU; cpu++ {
		config.GinkgoConfig.ParallelNode = cpu + 1
		config.GinkgoConfig.ParallelTotal = t.numCPU

		args := config.BuildFlagArgs("ginkgo", config.GinkgoConfig, config.DefaultReporterConfig)
		args = append(args, t.commonArgs(suite)...)

		buffer := new(bytes.Buffer)
		t.reports = append(t.reports, buffer)

		go t.runCommand(suite.Path, args, nil, buffer, completions)
	}

	passed := true

	for cpu := 0; cpu < t.numCPU; cpu++ {
		passed = <-completions && passed
	}

	for _, report := range t.reports {
		fmt.Print(report.String())
	}
	os.Stdout.Sync()

	return passed
}

func (t *testRunner) runAndStreamParallelGinkgoSuite(suite *testsuite.TestSuite) bool {
	result := make(chan bool, 0)
	stenographer := stenographer.New(!config.DefaultReporterConfig.NoColor)
	aggregator := aggregator.NewAggregator(t.numCPU, result, config.DefaultReporterConfig, stenographer)

	server, err := remote.NewServer()
	if err != nil {
		panic("Failed to start parallel spec server")
	}

	server.RegisterReporters(aggregator)
	server.Start()

	serverAddress := server.Address()

	completions := make(chan bool)

	for cpu := 0; cpu < t.numCPU; cpu++ {
		config.GinkgoConfig.ParallelNode = cpu + 1
		config.GinkgoConfig.ParallelTotal = t.numCPU

		args := config.BuildFlagArgs("ginkgo", config.GinkgoConfig, config.DefaultReporterConfig)
		args = append(args, t.commonArgs(suite)...)

		env := os.Environ()
		env = append(env, fmt.Sprintf("GINKGO_REMOTE_REPORTING_SERVER=%s", serverAddress))

		buffer := new(bytes.Buffer)
		t.reports = append(t.reports, buffer)

		go t.runCommand(suite.Path, args, env, buffer, completions)
	}

	for cpu := 0; cpu < t.numCPU; cpu++ {
		<-completions
	}

	//all test processes are done, at this point
	//we should be able to wait for the aggregator to tell us that it's done

	var passed = false
	select {
	case passed = <-result:
		//the aggregator is done and can tell us whether or not the suite passed
	case <-time.After(time.Second):
		//the aggregator never got back to us!  something must have gone wrong
		fmt.Println("")
		fmt.Println("")
		fmt.Println("   ----------------------------------------------------------- ")
		fmt.Println("  |                                                           |")
		fmt.Println("  |  Ginkgo timed out waiting for all parallel nodes to end!  |")
		fmt.Println("  |  Here is some salvaged output:                            |")
		fmt.Println("  |                                                           |")
		fmt.Println("   ----------------------------------------------------------- ")
		fmt.Println("")
		fmt.Println("")

		os.Stdout.Sync()

		time.Sleep(time.Second)

		for _, report := range t.reports {
			fmt.Print(report.String())
		}

		os.Stdout.Sync()
	}

	server.Stop()

	return passed
}

func (t *testRunner) runSerialGinkgoSuite(suite *testsuite.TestSuite) bool {
	args := config.BuildFlagArgs("ginkgo", config.GinkgoConfig, config.DefaultReporterConfig)
	args = append(args, t.commonArgs(suite)...)
	return t.runCommand(suite.Path, args, nil, os.Stdout, nil)
}

func (t *testRunner) runGoTestSuite(suite *testsuite.TestSuite) bool {
	args := t.commonArgs(suite)
	return t.runCommand(suite.Path, args, nil, os.Stdout, nil)
}

func (t *testRunner) commonArgs(suite *testsuite.TestSuite) []string {
	args := []string{}
	if t.race {
		args = append(args, "--race")
	}
	if t.cover {
		args = append([]string{"--cover", "--coverprofile=" + suite.PackageName + ".coverprofile"})
	}
	return args
}

func (t *testRunner) runCommand(path string, args []string, env []string, stream io.Writer, completions chan bool) bool {
	args = append([]string{"test", "-v", "-timeout=24h", path}, args...)

	cmd := exec.Command("go", args...)
	cmd.Env = env

	//THIS NEEDS TO BE REMOVED AFTER THE COMAND EXITS
	//ALSO: NOT THREAD SAFE!
	t.executedCommands = append(t.executedCommands, cmd)

	doneStreaming := make(chan bool, 2)
	streamPipe := func(pipe io.ReadCloser) {
		io.Copy(stream, pipe)
		doneStreaming <- true
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	go streamPipe(stdout)
	go streamPipe(stderr)

	err := cmd.Start()
	if err != nil {
		os.Exit(1)
	}

	<-doneStreaming
	<-doneStreaming

	err = cmd.Wait()
	if completions != nil {
		completions <- (err == nil)
	}
	return err == nil
}

func (t *testRunner) registerSignalHandler() {
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, os.Kill)

		select {
		case sig := <-c:
			for _, cmd := range t.executedCommands {
				if cmd.Process != nil {
					cmd.Process.Signal(sig)
				}
			}
			os.Exit(1)
		}
	}()
}
