package taskrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	log "github.com/hashicorp/go-hclog"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/hcl2/hcldec"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/allocrunner/taskrunner/restarts"
	"github.com/hashicorp/nomad/client/allocrunner/taskrunner/state"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/consul"
	"github.com/hashicorp/nomad/client/devicemanager"
	cinterfaces "github.com/hashicorp/nomad/client/interfaces"
	"github.com/hashicorp/nomad/client/pluginmanager/drivermanager"
	cstate "github.com/hashicorp/nomad/client/state"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	bstructs "github.com/hashicorp/nomad/plugins/base/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/helper/pluginutils/hclspecutils"
	"github.com/hashicorp/nomad/helper/pluginutils/hclutils"
)

const (
	// defaultMaxEvents is the default max capacity for task events on the
	// task state. Overrideable for testing.
	defaultMaxEvents = 10

	// killBackoffBaseline is the baseline time for exponential backoff while
	// killing a task.
	killBackoffBaseline = 5 * time.Second

	// killBackoffLimit is the limit of the exponential backoff for killing
	// the task.
	killBackoffLimit = 2 * time.Minute

	// killFailureLimit is how many times we will attempt to kill a task before
	// giving up and potentially leaking resources.
	killFailureLimit = 5

	// triggerUpdatechCap is the capacity for the triggerUpdateCh used for
	// triggering updates. It should be exactly 1 as even if multiple
	// updates have come in since the last one was handled, we only need to
	// handle the last one.
	triggerUpdateChCap = 1
)

type TaskRunner struct {
	// allocID, taskName, taskLeader, and taskResources are immutable so these fields may
	// be accessed without locks
	allocID       string
	taskName      string
	taskLeader    bool
	taskResources *structs.AllocatedTaskResources

	alloc     *structs.Allocation
	allocLock sync.Mutex

	clientConfig *config.Config

	// stateUpdater is used to emit updated task state
	stateUpdater interfaces.TaskStateHandler

	// state captures the state of the task for updating the allocation
	// Must acquire stateLock to access.
	state *structs.TaskState

	// localState captures the node-local state of the task for when the
	// Nomad agent restarts.
	// Must acquire stateLock to access.
	localState *state.LocalState

	// stateLock must be acquired when accessing state or localState.
	stateLock sync.RWMutex

	// stateDB is for persisting localState and taskState
	stateDB cstate.StateDB

	// shutdownCtx is used to exit the TaskRunner *without* affecting task state.
	shutdownCtx context.Context

	// shutdownCtxCancel causes the TaskRunner to exit immediately without
	// affecting task state. Useful for testing or graceful agent shutdown.
	shutdownCtxCancel context.CancelFunc

	// killCtx is the task runner's context representing the tasks's lifecycle.
	// The context is canceled when the task is killed.
	killCtx context.Context

	// killCtxCancel is called when killing a task.
	killCtxCancel context.CancelFunc

	// killErr is populated when killing a task. Access should be done use the
	// getter/setter
	killErr     error
	killErrLock sync.Mutex

	// Logger is the logger for the task runner.
	logger log.Logger

	// triggerUpdateCh is ticked whenever update hooks need to be run and
	// must be created with cap=1 to signal a pending update and prevent
	// callers from deadlocking if the receiver has exited.
	triggerUpdateCh chan struct{}

	// waitCh is closed when the task runner has transitioned to a terminal
	// state
	waitCh chan struct{}

	// driver is the driver for the task.
	driver drivers.DriverPlugin

	// driverCapabilities is the set capabilities the driver supports
	driverCapabilities *drivers.Capabilities

	// taskSchema is the hcl spec for the task driver configuration
	taskSchema hcldec.Spec

	// handleLock guards access to handle and handleResult
	handleLock sync.Mutex

	// handle to the running driver
	handle *DriverHandle

	// task is the task being run
	task     *structs.Task
	taskLock sync.RWMutex

	// taskDir is the directory structure for this task.
	taskDir *allocdir.TaskDir

	// envBuilder is used to build the task's environment
	envBuilder *taskenv.Builder

	// restartTracker is used to decide if the task should be restarted.
	restartTracker *restarts.RestartTracker

	// runnerHooks are task runner lifecycle hooks that should be run on state
	// transistions.
	runnerHooks []interfaces.TaskHook

	// hookResources captures the resources provided by hooks
	hookResources *hookResources

	// consulClient is the client used by the consul service hook for
	// registering services and checks
	consulClient consul.ConsulServiceAPI

	// vaultClient is the client to use to derive and renew Vault tokens
	vaultClient vaultclient.VaultClient

	// vaultToken is the current Vault token. It should be accessed with the
	// getter.
	vaultToken     string
	vaultTokenLock sync.Mutex

	// baseLabels are used when emitting tagged metrics. All task runner metrics
	// will have these tags, and optionally more.
	baseLabels []metrics.Label

	// logmonHookConfig is used to get the paths to the stdout and stderr fifos
	// to be passed to the driver for task logging
	logmonHookConfig *logmonHookConfig

	// resourceUsage is written via UpdateStats and read via
	// LatestResourceUsage. May be nil at all times.
	resourceUsage     *cstructs.TaskResourceUsage
	resourceUsageLock sync.Mutex

	// deviceStatsReporter is used to lookup resource usage for alloc devices
	deviceStatsReporter cinterfaces.DeviceStatsReporter

	// devicemanager is used to mount devices as well as lookup device
	// statistics
	devicemanager devicemanager.Manager

	// driverManager is used to dispense driver plugins and register event
	// handlers
	driverManager drivermanager.Manager

	// runLaunched marks whether the Run goroutine has been started. It should
	// be accessed via helpers
	runLaunched     bool
	runLaunchedLock sync.Mutex

	// maxEvents is the capacity of the TaskEvents on the TaskState.
	// Defaults to defaultMaxEvents but overrideable for testing.
	maxEvents int
}

type Config struct {
	Alloc        *structs.Allocation
	ClientConfig *config.Config
	Consul       consul.ConsulServiceAPI
	Task         *structs.Task
	TaskDir      *allocdir.TaskDir
	Logger       log.Logger

	// Vault is the client to use to derive and renew Vault tokens
	Vault vaultclient.VaultClient

	// StateDB is used to store and restore state.
	StateDB cstate.StateDB

	// StateUpdater is used to emit updated task state
	StateUpdater interfaces.TaskStateHandler

	// deviceStatsReporter is used to lookup resource usage for alloc devices
	DeviceStatsReporter cinterfaces.DeviceStatsReporter

	// DeviceManager is used to mount devices as well as lookup device
	// statistics
	DeviceManager devicemanager.Manager

	// DriverManager is used to dispense driver plugins and register event
	// handlers
	DriverManager drivermanager.Manager
}

func NewTaskRunner(config *Config) (*TaskRunner, error) {
	// Create a context for causing the runner to exit
	trCtx, trCancel := context.WithCancel(context.Background())

	// Create a context for killing the runner
	killCtx, killCancel := context.WithCancel(context.Background())

	// Initialize the environment builder
	envBuilder := taskenv.NewBuilder(
		config.ClientConfig.Node,
		config.Alloc,
		config.Task,
		config.ClientConfig.Region,
	)

	// Initialize state from alloc if it is set
	tstate := structs.NewTaskState()
	if ts := config.Alloc.TaskStates[config.Task.Name]; ts != nil {
		tstate = ts.Copy()
	}

	tr := &TaskRunner{
		alloc:               config.Alloc,
		allocID:             config.Alloc.ID,
		clientConfig:        config.ClientConfig,
		task:                config.Task,
		taskDir:             config.TaskDir,
		taskName:            config.Task.Name,
		taskLeader:          config.Task.Leader,
		envBuilder:          envBuilder,
		consulClient:        config.Consul,
		vaultClient:         config.Vault,
		state:               tstate,
		localState:          state.NewLocalState(),
		stateDB:             config.StateDB,
		stateUpdater:        config.StateUpdater,
		deviceStatsReporter: config.DeviceStatsReporter,
		killCtx:             killCtx,
		killCtxCancel:       killCancel,
		shutdownCtx:         trCtx,
		shutdownCtxCancel:   trCancel,
		triggerUpdateCh:     make(chan struct{}, triggerUpdateChCap),
		waitCh:              make(chan struct{}),
		devicemanager:       config.DeviceManager,
		driverManager:       config.DriverManager,
		maxEvents:           defaultMaxEvents,
	}

	// Create the logger based on the allocation ID
	tr.logger = config.Logger.Named("task_runner").With("task", config.Task.Name)

	// Pull out the task's resources
	ares := tr.alloc.AllocatedResources
	if ares != nil {
		tres, ok := ares.Tasks[tr.taskName]
		if !ok {
			return nil, fmt.Errorf("no task resources found on allocation")
		}
		tr.taskResources = tres
	} else {
		// COMPAT(0.10): Upgrade from old resources to new resources
		// Grab the old task resources
		oldTr, ok := tr.alloc.TaskResources[tr.taskName]
		if !ok {
			return nil, fmt.Errorf("no task resources found on allocation")
		}

		// Convert the old to new
		tr.taskResources = &structs.AllocatedTaskResources{
			Cpu: structs.AllocatedCpuResources{
				CpuShares: int64(oldTr.CPU),
			},
			Memory: structs.AllocatedMemoryResources{
				MemoryMB: int64(oldTr.MemoryMB),
			},
			Networks: oldTr.Networks,
		}
	}

	// Build the restart tracker.
	tg := tr.alloc.Job.LookupTaskGroup(tr.alloc.TaskGroup)
	if tg == nil {
		tr.logger.Error("alloc missing task group")
		return nil, fmt.Errorf("alloc missing task group")
	}
	tr.restartTracker = restarts.NewRestartTracker(tg.RestartPolicy, tr.alloc.Job.Type)

	// Get the driver
	if err := tr.initDriver(); err != nil {
		tr.logger.Error("failed to create driver", "error", err)
		return nil, err
	}

	// Initialize the runners hooks.
	tr.initHooks()

	// Initialize base labels
	tr.initLabels()

	// Initialize initial task received event
	tr.appendEvent(structs.NewTaskEvent(structs.TaskReceived))

	return tr, nil
}

func (tr *TaskRunner) initLabels() {
	alloc := tr.Alloc()
	tr.baseLabels = []metrics.Label{
		{
			Name:  "job",
			Value: alloc.Job.Name,
		},
		{
			Name:  "task_group",
			Value: alloc.TaskGroup,
		},
		{
			Name:  "alloc_id",
			Value: tr.allocID,
		},
		{
			Name:  "task",
			Value: tr.taskName,
		},
	}

	if tr.alloc.Job.ParentID != "" {
		tr.baseLabels = append(tr.baseLabels, metrics.Label{
			Name:  "parent_id",
			Value: tr.alloc.Job.ParentID,
		})
		if strings.Contains(tr.alloc.Job.Name, "/dispatch-") {
			tr.baseLabels = append(tr.baseLabels, metrics.Label{
				Name:  "dispatch_id",
				Value: strings.Split(tr.alloc.Job.Name, "/dispatch-")[1],
			})
		}
		if strings.Contains(tr.alloc.Job.Name, "/periodic-") {
			tr.baseLabels = append(tr.baseLabels, metrics.Label{
				Name:  "periodic_id",
				Value: strings.Split(tr.alloc.Job.Name, "/periodic-")[1],
			})
		}
	}
}

// Run the TaskRunner. Starts the user's task or reattaches to a restored task.
// Run closes WaitCh when it exits. Should be started in a goroutine.
func (tr *TaskRunner) Run() {
	// Mark that the run routine has been launched so that other functions can
	// decide to use the wait channel or not.
	tr.setRunLaunched()

	defer close(tr.waitCh)
	var result *drivers.ExitResult

	// Updates are handled asynchronously with the other hooks but each
	// triggered update - whether due to alloc updates or a new vault token
	// - should be handled serially.
	go tr.handleUpdates()

MAIN:
	for {
		select {
		case <-tr.killCtx.Done():
			tr.handleKill()
			break MAIN
		case <-tr.shutdownCtx.Done():
			// TaskRunner was told to exit immediately
			return
		default:
		}

		// Run the prestart hooks
		if err := tr.prestart(); err != nil {
			tr.logger.Error("prestart failed", "error", err)
			tr.restartTracker.SetStartError(err)
			goto RESTART
		}

		select {
		case <-tr.killCtx.Done():
			tr.handleKill()
			break MAIN
		case <-tr.shutdownCtx.Done():
			// TaskRunner was told to exit immediately
			return
		default:
		}

		// Run the task
		if err := tr.runDriver(); err != nil {
			tr.logger.Error("running driver failed", "error", err)
			tr.EmitEvent(structs.NewTaskEvent(structs.TaskDriverFailure).SetDriverError(err))
			tr.restartTracker.SetStartError(err)
			goto RESTART
		}

		// Run the poststart hooks
		if err := tr.poststart(); err != nil {
			tr.logger.Error("poststart failed", "error", err)
		}

		// Grab the result proxy and wait for task to exit
	WAIT:
		{
			handle := tr.getDriverHandle()
			result = nil

			// Do *not* use tr.killCtx here as it would cause
			// Wait() to unblock before the task exits when Kill()
			// is called.
			if resultCh, err := handle.WaitCh(context.Background()); err != nil {
				tr.logger.Error("wait task failed", "error", err)
			} else {
				select {
				case <-tr.killCtx.Done():
					// We can go through the normal should restart check since
					// the restart tracker knowns it is killed
					result = tr.handleKill()
				case <-tr.shutdownCtx.Done():
					// TaskRunner was told to exit immediately
					return
				case result = <-resultCh:
				}

				// WaitCh returned a result
				if retryWait := tr.handleTaskExitResult(result); retryWait {
					goto WAIT
				}
			}
		}

		// Clear the handle
		tr.clearDriverHandle()

		// Store the wait result on the restart tracker
		tr.restartTracker.SetExitResult(result)

		if err := tr.exited(); err != nil {
			tr.logger.Error("exited hooks failed", "error", err)
		}

	RESTART:
		restart, restartDelay := tr.shouldRestart()
		if !restart {
			break MAIN
		}

		// Actually restart by sleeping and also watching for destroy events
		select {
		case <-time.After(restartDelay):
		case <-tr.killCtx.Done():
			tr.logger.Trace("task killed between restarts", "delay", restartDelay)
			tr.handleKill()
			break MAIN
		case <-tr.shutdownCtx.Done():
			// TaskRunner was told to exit immediately
			return
		}
	}

	// Mark the task as dead
	tr.UpdateState(structs.TaskStateDead, nil)

	// Run the stop hooks
	if err := tr.stop(); err != nil {
		tr.logger.Error("stop failed", "error", err)
	}

	tr.logger.Debug("task run loop exiting")
}

// handleTaskExitResult handles the results returned by the task exiting. If
// retryWait is true, the caller should attempt to wait on the task again since
// it has not actually finished running. This can happen if the driver plugin
// has exited.
func (tr *TaskRunner) handleTaskExitResult(result *drivers.ExitResult) (retryWait bool) {
	if result == nil {
		return false
	}

	if result.Err == bstructs.ErrPluginShutdown {
		dn := tr.Task().Driver
		tr.logger.Debug("driver plugin has shutdown; attempting to recover task", "driver", dn)

		// Initialize a new driver handle
		if err := tr.initDriver(); err != nil {
			tr.logger.Error("failed to initialize driver after it exited unexpectedly", "error", err, "driver", dn)
			return false
		}

		// Try to restore the handle
		tr.stateLock.RLock()
		h := tr.localState.TaskHandle
		net := tr.localState.DriverNetwork
		tr.stateLock.RUnlock()
		if !tr.restoreHandle(h, net) {
			tr.logger.Error("failed to restore handle on driver after it exited unexpectedly", "driver", dn)
			return false
		}

		tr.logger.Debug("task successfully recovered on driver", "driver", dn)
		return true
	}

	event := structs.NewTaskEvent(structs.TaskTerminated).
		SetExitCode(result.ExitCode).
		SetSignal(result.Signal).
		SetOOMKilled(result.OOMKilled).
		SetExitMessage(result.Err)

	tr.EmitEvent(event)

	if result.OOMKilled && !tr.clientConfig.DisableTaggedMetrics {
		metrics.IncrCounterWithLabels([]string{"client", "allocs", "oom_killed"}, 1, tr.baseLabels)
	}

	return false
}

// handleUpdates runs update hooks when triggerUpdateCh is ticked and exits
// when Run has returned. Should only be run in a goroutine from Run.
func (tr *TaskRunner) handleUpdates() {
	for {
		select {
		case <-tr.triggerUpdateCh:
		case <-tr.waitCh:
			return
		}

		// Non-terminal update; run hooks
		tr.updateHooks()
	}
}

// shouldRestart determines whether the task should be restarted and updates
// the task state unless the task is killed or terminated.
func (tr *TaskRunner) shouldRestart() (bool, time.Duration) {
	// Determine if we should restart
	state, when := tr.restartTracker.GetState()
	reason := tr.restartTracker.GetReason()
	switch state {
	case structs.TaskKilled:
		// Never restart an explicitly killed task. Kill method handles
		// updating the server.
		tr.EmitEvent(structs.NewTaskEvent(state))
		return false, 0
	case structs.TaskNotRestarting, structs.TaskTerminated:
		tr.logger.Info("not restarting task", "reason", reason)
		if state == structs.TaskNotRestarting {
			tr.UpdateState(structs.TaskStateDead, structs.NewTaskEvent(structs.TaskNotRestarting).SetRestartReason(reason).SetFailsTask())
		}
		return false, 0
	case structs.TaskRestarting:
		tr.logger.Info("restarting task", "reason", reason, "delay", when)
		tr.UpdateState(structs.TaskStatePending, structs.NewTaskEvent(structs.TaskRestarting).SetRestartDelay(when).SetRestartReason(reason))
		return true, when
	default:
		tr.logger.Error("restart tracker returned unknown state", "state", state)
		return true, when
	}
}

// runDriver runs the driver and waits for it to exit
func (tr *TaskRunner) runDriver() error {

	taskConfig := tr.buildTaskConfig()

	// Build hcl context variables
	vars, errs, err := tr.envBuilder.Build().AllValues()
	if err != nil {
		return fmt.Errorf("error building environment variables: %v", err)
	}

	// Handle per-key errors
	if len(errs) > 0 {
		keys := make([]string, 0, len(errs))
		for k, err := range errs {
			keys = append(keys, k)

			if tr.logger.IsTrace() {
				// Verbosely log every diagnostic for debugging
				tr.logger.Trace("error building environment variables", "key", k, "error", err)
			}
		}

		tr.logger.Warn("some environment variables not available for rendering", "keys", strings.Join(keys, ", "))
	}

	evalCtx := &hcl.EvalContext{
		Variables: vars,
		Functions: hclutils.GetStdlibFuncs(),
	}

	val, diag := hclutils.ParseHclInterface(tr.task.Config, tr.taskSchema, evalCtx)
	if diag.HasErrors() {
		return multierror.Append(errors.New("failed to parse config"), diag.Errs()...)
	}

	if err := taskConfig.EncodeDriverConfig(val); err != nil {
		return fmt.Errorf("failed to encode driver config: %v", err)
	}

	// If there's already a task handle (eg from a Restore) there's nothing
	// to do except update state.
	if tr.getDriverHandle() != nil {
		// Ensure running state is persisted but do *not* append a new
		// task event as restoring is a client event and not relevant
		// to a task's lifecycle.
		if err := tr.updateStateImpl(structs.TaskStateRunning); err != nil {
			//TODO return error and destroy task to avoid an orphaned task?
			tr.logger.Warn("error persisting task state", "error", err)
		}
		return nil
	}

	// Start the job if there's no existing handle (or if RecoverTask failed)
	handle, net, err := tr.driver.StartTask(taskConfig)
	if err != nil {
		// The plugin has died, try relaunching it
		if err == bstructs.ErrPluginShutdown {
			tr.logger.Info("failed to start task because plugin shutdown unexpectedly; attempting to recover")
			if err := tr.initDriver(); err != nil {
				return fmt.Errorf("failed to initialize driver after it exited unexpectedly: %v", err)
			}

			handle, net, err = tr.driver.StartTask(taskConfig)
			if err != nil {
				return fmt.Errorf("failed to start task after driver exited unexpectedly: %v", err)
			}
		} else {
			return fmt.Errorf("driver start failed: %v", err)
		}
	}

	tr.stateLock.Lock()
	tr.localState.TaskHandle = handle
	tr.localState.DriverNetwork = net
	if err := tr.stateDB.PutTaskRunnerLocalState(tr.allocID, tr.taskName, tr.localState); err != nil {
		//TODO Nomad will be unable to restore this task; try to kill
		//     it now and fail? In general we prefer to leave running
		//     tasks running even if the agent encounters an error.
		tr.logger.Warn("error persisting local task state; may be unable to restore after a Nomad restart",
			"error", err, "task_id", handle.Config.ID)
	}
	tr.stateLock.Unlock()

	tr.setDriverHandle(NewDriverHandle(tr.driver, taskConfig.ID, tr.Task(), net))

	// Emit an event that we started
	tr.UpdateState(structs.TaskStateRunning, structs.NewTaskEvent(structs.TaskStarted))
	return nil
}

// initDriver retrives the DriverPlugin from the plugin loader for this task
func (tr *TaskRunner) initDriver() error {
	driver, err := tr.driverManager.Dispense(tr.Task().Driver)
	if err != nil {
		return err
	}
	tr.driver = driver

	schema, err := tr.driver.TaskConfigSchema()
	if err != nil {
		return err
	}
	spec, diag := hclspecutils.Convert(schema)
	if diag.HasErrors() {
		return multierror.Append(errors.New("failed to convert task schema"), diag.Errs()...)
	}
	tr.taskSchema = spec

	caps, err := tr.driver.Capabilities()
	if err != nil {
		return err
	}
	tr.driverCapabilities = caps

	return nil
}

// handleKill is used to handle the a request to kill a task. It will return
//// the handle exit result if one is available and store any error in the task
//// runner killErr value.
func (tr *TaskRunner) handleKill() *drivers.ExitResult {
	// Run the pre killing hooks
	tr.preKill()

	// Tell the restart tracker that the task has been killed so it doesn't
	// attempt to restart it.
	tr.restartTracker.SetKilled()

	// Check it is running
	handle := tr.getDriverHandle()
	if handle == nil {
		return nil
	}

	// Kill the task using an exponential backoff in-case of failures.
	killErr := tr.killTask(handle)
	if killErr != nil {
		// We couldn't successfully destroy the resource created.
		tr.logger.Error("failed to kill task. Resources may have been leaked", "error", killErr)
		tr.setKillErr(killErr)
	}

	// Block until task has exited.
	waitCh, err := handle.WaitCh(tr.shutdownCtx)

	// The error should be nil or TaskNotFound, if it's something else then a
	// failure in the driver or transport layer occurred
	if err != nil {
		if err == drivers.ErrTaskNotFound {
			return nil
		}
		tr.logger.Error("failed to wait on task. Resources may have been leaked", "error", err)
		tr.setKillErr(killErr)
		return nil
	}

	select {
	case result := <-waitCh:
		return result
	case <-tr.shutdownCtx.Done():
		return nil
	}
}

// killTask kills the task handle. In the case that killing fails,
// killTask will retry with an exponential backoff and will give up at a
// given limit. Returns an error if the task could not be killed.
func (tr *TaskRunner) killTask(handle *DriverHandle) error {
	// Cap the number of times we attempt to kill the task.
	var err error
	for i := 0; i < killFailureLimit; i++ {
		if err = handle.Kill(); err != nil {
			if err == drivers.ErrTaskNotFound {
				tr.logger.Warn("couldn't find task to kill", "task_id", handle.ID())
				return nil
			}
			// Calculate the new backoff
			backoff := (1 << (2 * uint64(i))) * killBackoffBaseline
			if backoff > killBackoffLimit {
				backoff = killBackoffLimit
			}

			tr.logger.Error("failed to kill task", "backoff", backoff, "error", err)
			time.Sleep(backoff)
		} else {
			// Kill was successful
			return nil
		}
	}
	return err
}

// persistLocalState persists local state to disk synchronously.
func (tr *TaskRunner) persistLocalState() error {
	tr.stateLock.RLock()
	defer tr.stateLock.RUnlock()

	return tr.stateDB.PutTaskRunnerLocalState(tr.allocID, tr.taskName, tr.localState)
}

// buildTaskConfig builds a drivers.TaskConfig with an unique ID for the task.
// The ID is unique for every invocation, it is built from the alloc ID, task
// name and 8 random characters.
func (tr *TaskRunner) buildTaskConfig() *drivers.TaskConfig {
	task := tr.Task()
	alloc := tr.Alloc()
	invocationid := uuid.Generate()[:8]
	taskResources := tr.taskResources
	env := tr.envBuilder.Build()

	return &drivers.TaskConfig{
		ID:            fmt.Sprintf("%s/%s/%s", alloc.ID, task.Name, invocationid),
		Name:          task.Name,
		JobName:       alloc.Job.Name,
		TaskGroupName: alloc.TaskGroup,
		Resources: &drivers.Resources{
			NomadResources: taskResources,
			LinuxResources: &drivers.LinuxResources{
				MemoryLimitBytes: taskResources.Memory.MemoryMB * 1024 * 1024,
				CPUShares:        taskResources.Cpu.CpuShares,
				PercentTicks:     float64(taskResources.Cpu.CpuShares) / float64(tr.clientConfig.Node.NodeResources.Cpu.CpuShares),
			},
		},
		Devices:    tr.hookResources.getDevices(),
		Mounts:     tr.hookResources.getMounts(),
		Env:        env.Map(),
		DeviceEnv:  env.DeviceEnv(),
		User:       task.User,
		AllocDir:   tr.taskDir.AllocDir,
		StdoutPath: tr.logmonHookConfig.stdoutFifo,
		StderrPath: tr.logmonHookConfig.stderrFifo,
		AllocID:    tr.allocID,
	}
}

// Restore task runner state. Called by AllocRunner.Restore after NewTaskRunner
// but before Run so no locks need to be acquired.
func (tr *TaskRunner) Restore() error {
	ls, ts, err := tr.stateDB.GetTaskRunnerState(tr.allocID, tr.taskName)
	if err != nil {
		return err
	}

	if ls != nil {
		ls.Canonicalize()
		tr.localState = ls
	}

	if ts != nil {
		ts.Canonicalize()
		tr.state = ts
	}

	// If a TaskHandle was persisted, ensure it is valid or destroy it.
	if taskHandle := tr.localState.TaskHandle; taskHandle != nil {
		//TODO if RecoverTask returned the DriverNetwork we wouldn't
		//     have to persist it at all!
		tr.restoreHandle(taskHandle, tr.localState.DriverNetwork)
	}
	return nil
}

// restoreHandle ensures a TaskHandle is valid by calling Driver.RecoverTask
// and sets the driver handle. If the TaskHandle is not valid, DestroyTask is
// called.
func (tr *TaskRunner) restoreHandle(taskHandle *drivers.TaskHandle, net *drivers.DriverNetwork) (success bool) {
	// Ensure handle is well-formed
	if taskHandle.Config == nil {
		return true
	}

	if err := tr.driver.RecoverTask(taskHandle); err != nil {
		if tr.TaskState().State != structs.TaskStateRunning {
			// RecoverTask should fail if the Task wasn't running
			return true
		}

		tr.logger.Error("error recovering task; cleaning up",
			"error", err, "task_id", taskHandle.Config.ID)

		// Try to cleanup any existing task state in the plugin before restarting
		if err := tr.driver.DestroyTask(taskHandle.Config.ID, true); err != nil {
			// Ignore ErrTaskNotFound errors as ideally
			// this task has already been stopped and
			// therefore doesn't exist.
			if err != drivers.ErrTaskNotFound {
				tr.logger.Warn("error destroying unrecoverable task",
					"error", err, "task_id", taskHandle.Config.ID)
			}

			return false
		}

		return true
	}

	// Update driver handle on task runner
	tr.setDriverHandle(NewDriverHandle(tr.driver, taskHandle.Config.ID, tr.Task(), net))
	return true
}

// UpdateState sets the task runners allocation state and triggers a server
// update.
func (tr *TaskRunner) UpdateState(state string, event *structs.TaskEvent) {
	tr.stateLock.Lock()
	defer tr.stateLock.Unlock()

	if event != nil {
		tr.logger.Trace("setting task state", "state", state, "event", event.Type)

		// Append the event
		tr.appendEvent(event)
	}

	// Update the state
	if err := tr.updateStateImpl(state); err != nil {
		// Only log the error as we persistence errors should not
		// affect task state.
		tr.logger.Error("error persisting task state", "error", err, "event", event, "state", state)
	}

	// Notify the alloc runner of the transition
	tr.stateUpdater.TaskStateUpdated()
}

// updateStateImpl updates the in-memory task state and persists to disk.
func (tr *TaskRunner) updateStateImpl(state string) error {

	// Update the task state
	oldState := tr.state.State
	taskState := tr.state
	taskState.State = state

	// Handle the state transition.
	switch state {
	case structs.TaskStateRunning:
		// Capture the start time if it is just starting
		if oldState != structs.TaskStateRunning {
			taskState.StartedAt = time.Now().UTC()
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "running"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "running"}, 1)
			//}
		}
	case structs.TaskStateDead:
		// Capture the finished time if not already set
		if taskState.FinishedAt.IsZero() {
			taskState.FinishedAt = time.Now().UTC()
		}

		// Emitting metrics to indicate task complete and failures
		if taskState.Failed {
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "failed"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "failed"}, 1)
			//}
		} else {
			if !tr.clientConfig.DisableTaggedMetrics {
				metrics.IncrCounterWithLabels([]string{"client", "allocs", "complete"}, 1, tr.baseLabels)
			}
			//if r.config.BackwardsCompatibleMetrics {
			//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "complete"}, 1)
			//}
		}
	}

	// Persist the state and event
	return tr.stateDB.PutTaskState(tr.allocID, tr.taskName, taskState)
}

// EmitEvent appends a new TaskEvent to this task's TaskState. The actual
// TaskState.State (pending, running, dead) is not changed. Use UpdateState to
// transition states.
// Events are persisted locally and sent to the server, but errors are simply
// logged. Use AppendEvent to simply add a new event.
func (tr *TaskRunner) EmitEvent(event *structs.TaskEvent) {
	tr.stateLock.Lock()
	defer tr.stateLock.Unlock()

	tr.appendEvent(event)

	if err := tr.stateDB.PutTaskState(tr.allocID, tr.taskName, tr.state); err != nil {
		// Only a warning because the next event/state-transition will
		// try to persist it again.
		tr.logger.Warn("error persisting event", "error", err, "event", event)
	}

	// Notify the alloc runner of the event
	tr.stateUpdater.TaskStateUpdated()
}

// AppendEvent appends a new TaskEvent to this task's TaskState. The actual
// TaskState.State (pending, running, dead) is not changed. Use UpdateState to
// transition states.
// Events are persisted locally and errors are simply logged. Use EmitEvent
// also update AllocRunner.
func (tr *TaskRunner) AppendEvent(event *structs.TaskEvent) {
	tr.stateLock.Lock()
	defer tr.stateLock.Unlock()

	tr.appendEvent(event)

	if err := tr.stateDB.PutTaskState(tr.allocID, tr.taskName, tr.state); err != nil {
		// Only a warning because the next event/state-transition will
		// try to persist it again.
		tr.logger.Warn("error persisting event", "error", err, "event", event)
	}
}

// appendEvent to task's event slice. Caller must acquire stateLock.
func (tr *TaskRunner) appendEvent(event *structs.TaskEvent) error {
	// Ensure the event is populated with human readable strings
	event.PopulateEventDisplayMessage()

	// Propagate failure from event to task state
	if event.FailsTask {
		tr.state.Failed = true
	}

	// XXX This seems like a super awkward spot for this? Why not shouldRestart?
	// Update restart metrics
	if event.Type == structs.TaskRestarting {
		if !tr.clientConfig.DisableTaggedMetrics {
			metrics.IncrCounterWithLabels([]string{"client", "allocs", "restart"}, 1, tr.baseLabels)
		}
		//if r.config.BackwardsCompatibleMetrics {
		//metrics.IncrCounter([]string{"client", "allocs", r.alloc.Job.Name, r.alloc.TaskGroup, taskName, "restart"}, 1)
		//}
		tr.state.Restarts++
		tr.state.LastRestart = time.Unix(0, event.Time)
	}

	// Append event to slice
	appendTaskEvent(tr.state, event, tr.maxEvents)

	return nil
}

// WaitCh is closed when TaskRunner.Run exits.
func (tr *TaskRunner) WaitCh() <-chan struct{} {
	return tr.waitCh
}

// Update the running allocation with a new version received from the server.
// Calls Update hooks asynchronously with Run().
//
// This method is safe for calling concurrently with Run() and does not modify
// the passed in allocation.
func (tr *TaskRunner) Update(update *structs.Allocation) {
	task := update.LookupTask(tr.taskName)
	if task == nil {
		// This should not happen and likely indicates a bug in the
		// server or client.
		tr.logger.Error("allocation update is missing task; killing",
			"group", update.TaskGroup)
		te := structs.NewTaskEvent(structs.TaskKilled).
			SetKillReason("update missing task").
			SetFailsTask()
		tr.Kill(context.Background(), te)
		return
	}

	// Update tr.alloc
	tr.setAlloc(update, task)

	// Trigger update hooks if not terminal
	if !update.TerminalStatus() {
		tr.triggerUpdateHooks()
	}
}

// triggerUpdate if there isn't already an update pending. Should be called
// instead of calling updateHooks directly to serialize runs of update hooks.
// TaskRunner state should be updated prior to triggering update hooks.
//
// Does not block.
func (tr *TaskRunner) triggerUpdateHooks() {
	select {
	case tr.triggerUpdateCh <- struct{}{}:
	default:
		// already an update hook pending
	}
}

// Shutdown TaskRunner gracefully without affecting the state of the task.
// Shutdown blocks until the main Run loop exits.
func (tr *TaskRunner) Shutdown() {
	tr.logger.Trace("shutting down")
	tr.shutdownCtxCancel()

	<-tr.WaitCh()

	// Run shutdown hooks to cleanup
	tr.shutdownHooks()

	// Persist once more
	tr.persistLocalState()
}

// LatestResourceUsage returns the last resource utilization datapoint
// collected. May return nil if the task is not running or no resource
// utilization has been collected yet.
func (tr *TaskRunner) LatestResourceUsage() *cstructs.TaskResourceUsage {
	tr.resourceUsageLock.Lock()
	ru := tr.resourceUsage
	tr.resourceUsageLock.Unlock()

	// Look up device statistics lazily when fetched, as currently we do not emit any stats for them yet
	if ru != nil && tr.deviceStatsReporter != nil {
		deviceResources := tr.taskResources.Devices
		ru.ResourceUsage.DeviceStats = tr.deviceStatsReporter.LatestDeviceResourceStats(deviceResources)
	}
	return ru
}

// UpdateStats updates and emits the latest stats from the driver.
func (tr *TaskRunner) UpdateStats(ru *cstructs.TaskResourceUsage) {
	tr.resourceUsageLock.Lock()
	tr.resourceUsage = ru
	tr.resourceUsageLock.Unlock()
	if ru != nil {
		tr.emitStats(ru)
	}
}

//TODO Remove Backwardscompat or use tr.Alloc()?
func (tr *TaskRunner) setGaugeForMemory(ru *cstructs.TaskResourceUsage) {
	if !tr.clientConfig.DisableTaggedMetrics {
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "rss"},
			float32(ru.ResourceUsage.MemoryStats.RSS), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "cache"},
			float32(ru.ResourceUsage.MemoryStats.Cache), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "swap"},
			float32(ru.ResourceUsage.MemoryStats.Swap), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "usage"},
			float32(ru.ResourceUsage.MemoryStats.Usage), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "max_usage"},
			float32(ru.ResourceUsage.MemoryStats.MaxUsage), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "kernel_usage"},
			float32(ru.ResourceUsage.MemoryStats.KernelUsage), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "memory", "kernel_max_usage"},
			float32(ru.ResourceUsage.MemoryStats.KernelMaxUsage), tr.baseLabels)
	}

	if tr.clientConfig.BackwardsCompatibleMetrics {
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "rss"}, float32(ru.ResourceUsage.MemoryStats.RSS))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "cache"}, float32(ru.ResourceUsage.MemoryStats.Cache))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "swap"}, float32(ru.ResourceUsage.MemoryStats.Swap))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "usage"}, float32(ru.ResourceUsage.MemoryStats.Usage))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "max_usage"}, float32(ru.ResourceUsage.MemoryStats.MaxUsage))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "kernel_usage"}, float32(ru.ResourceUsage.MemoryStats.KernelUsage))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "memory", "kernel_max_usage"}, float32(ru.ResourceUsage.MemoryStats.KernelMaxUsage))
	}
}

//TODO Remove Backwardscompat or use tr.Alloc()?
func (tr *TaskRunner) setGaugeForCPU(ru *cstructs.TaskResourceUsage) {
	if !tr.clientConfig.DisableTaggedMetrics {
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "cpu", "total_percent"},
			float32(ru.ResourceUsage.CpuStats.Percent), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "cpu", "system"},
			float32(ru.ResourceUsage.CpuStats.SystemMode), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "cpu", "user"},
			float32(ru.ResourceUsage.CpuStats.UserMode), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "cpu", "throttled_time"},
			float32(ru.ResourceUsage.CpuStats.ThrottledTime), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "cpu", "throttled_periods"},
			float32(ru.ResourceUsage.CpuStats.ThrottledPeriods), tr.baseLabels)
		metrics.SetGaugeWithLabels([]string{"client", "allocs", "cpu", "total_ticks"},
			float32(ru.ResourceUsage.CpuStats.TotalTicks), tr.baseLabels)
	}

	if tr.clientConfig.BackwardsCompatibleMetrics {
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "cpu", "total_percent"}, float32(ru.ResourceUsage.CpuStats.Percent))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "cpu", "system"}, float32(ru.ResourceUsage.CpuStats.SystemMode))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "cpu", "user"}, float32(ru.ResourceUsage.CpuStats.UserMode))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "cpu", "throttled_time"}, float32(ru.ResourceUsage.CpuStats.ThrottledTime))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "cpu", "throttled_periods"}, float32(ru.ResourceUsage.CpuStats.ThrottledPeriods))
		metrics.SetGauge([]string{"client", "allocs", tr.alloc.Job.Name, tr.alloc.TaskGroup, tr.allocID, tr.taskName, "cpu", "total_ticks"}, float32(ru.ResourceUsage.CpuStats.TotalTicks))
	}
}

// emitStats emits resource usage stats of tasks to remote metrics collector
// sinks
func (tr *TaskRunner) emitStats(ru *cstructs.TaskResourceUsage) {
	if !tr.clientConfig.PublishAllocationMetrics {
		return
	}

	if ru.ResourceUsage.MemoryStats != nil {
		tr.setGaugeForMemory(ru)
	}

	if ru.ResourceUsage.CpuStats != nil {
		tr.setGaugeForCPU(ru)
	}
}

// appendTaskEvent updates the task status by appending the new event.
func appendTaskEvent(state *structs.TaskState, event *structs.TaskEvent, capacity int) {
	if state.Events == nil {
		state.Events = make([]*structs.TaskEvent, 1, capacity)
		state.Events[0] = event
		return
	}

	// If we hit capacity, then shift it.
	if len(state.Events) == capacity {
		old := state.Events
		state.Events = make([]*structs.TaskEvent, 0, capacity)
		state.Events = append(state.Events, old[1:]...)
	}

	state.Events = append(state.Events, event)
}