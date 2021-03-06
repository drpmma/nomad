package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/hashicorp/consul-template/signals"
	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/client/driver/executor"
	dstructs "github.com/hashicorp/nomad/client/driver/structs"
	"github.com/hashicorp/nomad/client/fingerprint"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/utils"
	"github.com/hashicorp/nomad/plugins/shared"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	"github.com/hashicorp/nomad/plugins/shared/loader"
)

const (
	// pluginName is the name of the plugin
	pluginName = "exec"

	// fingerprintPeriod is the interval at which the driver will send fingerprint responses
	fingerprintPeriod = 30 * time.Second
)

var (
	// PluginID is the exec plugin metadata registered in the plugin
	// catalog.
	PluginID = loader.PluginID{
		Name:       pluginName,
		PluginType: base.PluginTypeDriver,
	}

	// PluginConfig is the exec driver factory function registered in the
	// plugin catalog.
	PluginConfig = &loader.InternalPluginConfig{
		Config:  map[string]interface{}{},
		Factory: func(l hclog.Logger) interface{} { return NewExecDriver(l) },
	}

	// pluginInfo is the response returned for the PluginInfo RPC
	pluginInfo = &base.PluginInfoResponse{
		Type:             base.PluginTypeDriver,
		PluginApiVersion: "0.0.1",
		PluginVersion:    "0.1.0",
		Name:             pluginName,
	}

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a task within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"command": hclspec.NewAttr("command", "string", true),
		"args":    hclspec.NewAttr("args", "list(string)", false),
	})

	// capabilities is returned by the Capabilities RPC and indicates what
	// optional features this driver supports
	capabilities = &drivers.Capabilities{
		SendSignals: true,
		Exec:        true,
		FSIsolation: cstructs.FSIsolationChroot,
	}
)

// Driver fork/execs tasks using many of the underlying OS's isolation
// features where configured.
type Driver struct {
	// eventer is used to handle multiplexing of TaskEvents calls such that an
	// event can be broadcast to all callers
	eventer *eventer.Eventer

	// nomadConfig is the client config from nomad
	nomadConfig *base.ClientDriverConfig

	// tasks is the in memory datastore mapping taskIDs to driverHandles
	tasks *taskStore

	// ctx is the context for the driver. It is passed to other subsystems to
	// coordinate shutdown
	ctx context.Context

	// signalShutdown is called when the driver is shutting down and cancels the
	// ctx passed to any subsystems
	signalShutdown context.CancelFunc

	// logger will log to the Nomad agent
	logger hclog.Logger
}

// TaskConfig is the driver configuration of a task within a job
type TaskConfig struct {
	Command string   `codec:"command"`
	Args    []string `codec:"args"`
}

// TaskState is the state which is encoded in the handle returned in
// StartTask. This information is needed to rebuild the task state and handler
// during recovery.
type TaskState struct {
	ReattachConfig *shared.ReattachConfig
	TaskConfig     *drivers.TaskConfig
	Pid            int
	StartedAt      time.Time
}

// NewExecDriver returns a new DrivePlugin implementation
func NewExecDriver(logger hclog.Logger) drivers.DriverPlugin {
	ctx, cancel := context.WithCancel(context.Background())
	logger = logger.Named(pluginName)
	return &Driver{
		eventer:        eventer.NewEventer(ctx, logger),
		tasks:          newTaskStore(),
		ctx:            ctx,
		signalShutdown: cancel,
		logger:         logger,
	}
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) {
	return pluginInfo, nil
}

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) {
	return configSpec, nil
}

func (d *Driver) SetConfig(_ []byte, cfg *base.ClientAgentConfig) error {
	if cfg != nil {
		d.nomadConfig = cfg.Driver
	}
	return nil
}

func (d *Driver) Shutdown(ctx context.Context) error {
	d.signalShutdown()
	return nil
}

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) {
	return taskConfigSpec, nil
}

func (d *Driver) Capabilities() (*drivers.Capabilities, error) {
	return capabilities, nil
}

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go d.handleFingerprint(ctx, ch)
	return ch, nil

}
func (d *Driver) handleFingerprint(ctx context.Context, ch chan<- *drivers.Fingerprint) {
	defer close(ch)
	ticker := time.NewTimer(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(fingerprintPeriod)
			ch <- d.buildFingerprint()
		}
	}
}

func (d *Driver) buildFingerprint() *drivers.Fingerprint {
	if runtime.GOOS != "linux" {
		return &drivers.Fingerprint{
			Health:            drivers.HealthStateUndetected,
			HealthDescription: "exec driver unsupported on client OS",
		}
	}

	fp := &drivers.Fingerprint{
		Attributes:        map[string]string{},
		Health:            drivers.HealthStateHealthy,
		HealthDescription: "healthy",
	}

	mount, err := fingerprint.FindCgroupMountpointDir()
	if err != nil {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = "failed to discover cgroup mount point"
		d.logger.Warn(fp.HealthDescription, "error", err)
		return fp
	}

	if mount == "" {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = "requires cgroup"
		return fp
	}

	if !utils.IsUnixRoot() {
		fp.Health = drivers.HealthStateUnhealthy
		fp.HealthDescription = "requires root"
		return fp
	}

	fp.Attributes["driver.exec"] = "1"
	return fp
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil {
		return fmt.Errorf("handle cannot be nil")
	}

	// If already attached to handle there's nothing to recover.
	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		d.logger.Trace("nothing to recover; task already exists",
			"task_id", handle.Config.ID,
			"task_name", handle.Config.Name,
		)
		return nil
	}

	// Handle doesn't already exist, try to reattach
	var taskState TaskState
	if err := handle.GetDriverState(&taskState); err != nil {
		d.logger.Error("failed to decode task state from handle", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to decode task state from handle: %v", err)
	}

	// Create client for reattached executor
	plugRC, err := shared.ReattachConfigToGoPlugin(taskState.ReattachConfig)
	if err != nil {
		d.logger.Error("failed to build ReattachConfig from task state", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to build ReattachConfig from task state: %v", err)
	}

	pluginConfig := &plugin.ClientConfig{
		Reattach: plugRC,
	}

	exec, pluginClient, err := utils.CreateExecutorWithConfig(pluginConfig, os.Stderr)
	if err != nil {
		d.logger.Error("failed to reattach to executor", "error", err, "task_id", handle.Config.ID)
		return fmt.Errorf("failed to reattach to executor: %v", err)
	}

	h := &taskHandle{
		exec:         exec,
		pid:          taskState.Pid,
		pluginClient: pluginClient,
		taskConfig:   taskState.TaskConfig,
		procState:    drivers.TaskStateRunning,
		startedAt:    taskState.StartedAt,
		exitResult:   &drivers.ExitResult{},
	}

	d.tasks.Set(taskState.TaskConfig.ID, h)

	go h.run()
	return nil
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *cstructs.DriverNetwork, error) {
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}

	var driverConfig TaskConfig
	if err := cfg.DecodeDriverConfig(&driverConfig); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %v", err)
	}

	d.logger.Info("starting task", "driver_cfg", hclog.Fmt("%+v", driverConfig))
	handle := drivers.NewTaskHandle(pluginName)
	handle.Config = cfg

	pluginLogFile := filepath.Join(cfg.TaskDir().Dir, "executor.out")
	executorConfig := &dstructs.ExecutorConfig{
		LogFile:     pluginLogFile,
		LogLevel:    "debug",
		FSIsolation: true,
	}

	// TODO: best way to pass port ranges in from client config
	exec, pluginClient, err := utils.CreateExecutor(os.Stderr, hclog.Debug, d.nomadConfig, executorConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create executor: %v", err)
	}

	execCmd := &executor.ExecCommand{
		Cmd:            driverConfig.Command,
		Args:           driverConfig.Args,
		Env:            cfg.EnvList(),
		User:           cfg.User,
		ResourceLimits: true,
		Resources:      toExecResources(cfg.Resources),
		TaskDir:        cfg.TaskDir().Dir,
		StdoutPath:     cfg.StdoutPath,
		StderrPath:     cfg.StderrPath,
	}

	ps, err := exec.Launch(execCmd)
	if err != nil {
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to launch command with executor: %v", err)
	}

	h := &taskHandle{
		exec:         exec,
		pid:          ps.Pid,
		pluginClient: pluginClient,
		taskConfig:   cfg,
		procState:    drivers.TaskStateRunning,
		startedAt:    time.Now().Round(time.Millisecond),
		logger:       d.logger,
	}

	driverState := TaskState{
		ReattachConfig: shared.ReattachConfigFromGoPlugin(pluginClient.ReattachConfig()),
		Pid:            ps.Pid,
		TaskConfig:     cfg,
		StartedAt:      h.startedAt,
	}

	if err := handle.SetDriverState(&driverState); err != nil {
		d.logger.Error("failed to start task, error setting driver state", "error", err)
		exec.Shutdown("", 0)
		pluginClient.Kill()
		return nil, nil, fmt.Errorf("failed to set driver state: %v", err)
	}

	d.tasks.Set(cfg.ID, h)
	go h.run()
	return handle, nil, nil
}

func toExecResources(resources *drivers.Resources) *executor.Resources {
	if resources == nil || resources.NomadResources == nil {
		return nil
	}

	return &executor.Resources{
		CPU:      resources.NomadResources.CPU,
		MemoryMB: resources.NomadResources.MemoryMB,
		DiskMB:   resources.NomadResources.DiskMB,
	}

}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	ch := make(chan *drivers.ExitResult)
	go d.handleWait(ctx, handle, ch)

	return ch, nil
}

func (d *Driver) handleWait(ctx context.Context, handle *taskHandle, ch chan *drivers.ExitResult) {
	defer close(ch)
	var result *drivers.ExitResult
	ps, err := handle.exec.Wait()
	if err != nil {
		result = &drivers.ExitResult{
			Err: fmt.Errorf("executor: error waiting on process: %v", err),
		}
	} else {
		result = &drivers.ExitResult{
			ExitCode: ps.ExitCode,
			Signal:   ps.Signal,
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-d.ctx.Done():
		return
	case ch <- result:
	}
}

func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if err := handle.exec.Shutdown(signal, timeout); err != nil {
		if handle.pluginClient.Exited() {
			return nil
		}
		return fmt.Errorf("executor Shutdown failed: %v", err)
	}

	return nil
}

func (d *Driver) DestroyTask(taskID string, force bool) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	if handle.IsRunning() && !force {
		return fmt.Errorf("cannot destroy running task")
	}

	if !handle.pluginClient.Exited() {
		if handle.IsRunning() {
			if err := handle.exec.Shutdown("", 0); err != nil {
				handle.logger.Error("destroying executor failed", "err", err)
			}
		}

		handle.pluginClient.Kill()
	}

	d.tasks.Delete(taskID)
	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.TaskStatus(), nil
}

func (d *Driver) TaskStats(taskID string) (*cstructs.TaskResourceUsage, error) {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	return handle.exec.Stats()
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}

	sig := os.Interrupt
	if s, ok := signals.SignalLookup[signal]; ok {
		d.logger.Warn("signal to send to task unknown, using SIGINT", "signal", signal, "task_id", handle.taskConfig.ID)
		sig = s
	}
	return handle.exec.Signal(sig)
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("error cmd must have atleast one value")
	}
	handle, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}

	args := []string{}
	if len(cmd) > 1 {
		args = cmd[1:]
	}

	out, exitCode, err := handle.exec.Exec(time.Now().Add(timeout), cmd[0], args)
	if err != nil {
		return nil, err
	}

	return &drivers.ExecTaskResult{
		Stdout: out,
		ExitResult: &drivers.ExitResult{
			ExitCode: exitCode,
		},
	}, nil
}
