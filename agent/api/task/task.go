// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package task

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	"github.com/aws/amazon-ecs-agent/agent/api/serviceconnect"
	"github.com/aws/amazon-ecs-agent/agent/config"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient"
	"github.com/aws/amazon-ecs-agent/agent/dockerclient/dockerapi"
	"github.com/aws/amazon-ecs-agent/agent/taskresource"
	"github.com/aws/amazon-ecs-agent/agent/taskresource/asmauth"
	"github.com/aws/amazon-ecs-agent/agent/taskresource/asmsecret"
	"github.com/aws/amazon-ecs-agent/agent/taskresource/credentialspec"
	"github.com/aws/amazon-ecs-agent/agent/taskresource/envFiles"
	"github.com/aws/amazon-ecs-agent/agent/taskresource/firelens"
	"github.com/aws/amazon-ecs-agent/agent/taskresource/ssmsecret"
	resourcestatus "github.com/aws/amazon-ecs-agent/agent/taskresource/status"
	resourcetype "github.com/aws/amazon-ecs-agent/agent/taskresource/types"
	taskresourcevolume "github.com/aws/amazon-ecs-agent/agent/taskresource/volume"
	"github.com/aws/amazon-ecs-agent/agent/utils"
	"github.com/aws/amazon-ecs-agent/ecs-agent/acs/model/ecsacs"
	"github.com/aws/amazon-ecs-agent/ecs-agent/api/container/restart"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	apierrors "github.com/aws/amazon-ecs-agent/ecs-agent/api/errors"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/credentials"
	"github.com/aws/amazon-ecs-agent/ecs-agent/ipcompatibility"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger"
	"github.com/aws/amazon-ecs-agent/ecs-agent/logger/field"
	nlappmesh "github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/appmesh"
	ni "github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/networkinterface"
	commonutils "github.com/aws/amazon-ecs-agent/ecs-agent/utils"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils/arn"
	"github.com/aws/amazon-ecs-agent/ecs-agent/utils/ttime"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
)

const (
	// NetworkPauseContainerName is the internal name for the pause container
	NetworkPauseContainerName = "~internal~ecs~pause"
	// ServiceConnectPauseContainerNameFormat is the naming format for SC pause containers
	ServiceConnectPauseContainerNameFormat = "~internal~ecs~pause-%s"

	// NamespacePauseContainerName is the internal name for the IPC resource namespace and/or
	// PID namespace sharing pause container
	NamespacePauseContainerName = "~internal~ecs~pause~namespace"

	emptyHostVolumeName = "~internal~ecs-emptyvolume-source"

	// awsSDKCredentialsRelativeURIPathEnvironmentVariableName defines the name of the environment
	// variable in containers' config, which will be used by the AWS SDK to fetch
	// credentials.
	awsSDKCredentialsRelativeURIPathEnvironmentVariableName = "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"

	NvidiaVisibleDevicesEnvVar = "NVIDIA_VISIBLE_DEVICES"
	GPUAssociationType         = "gpu"

	// neuronRuntime is the name of the neuron docker runtime.
	neuronRuntime = "neuron"

	ContainerOrderingCreateCondition  = "CREATE"
	ContainerOrderingStartCondition   = "START"
	ContainerOrderingHealthyCondition = "HEALTHY"

	// networkModeNone specifies the string used to define the `none` docker networking mode
	networkModeNone = "none"
	// dockerMappingContainerPrefix specifies the prefix string used for setting the
	// container's option (network, ipc, or pid) to that of another existing container
	dockerMappingContainerPrefix = "container:"

	// awslogsCredsEndpointOpt is the awslogs option that is used to pass in a
	// http endpoint for authentication
	awslogsCredsEndpointOpt = "awslogs-credentials-endpoint"
	// These contants identify the docker flag options
	pidModeHost     = "host"
	pidModeTask     = "task"
	ipcModeHost     = "host"
	ipcModeTask     = "task"
	ipcModeSharable = "shareable"
	ipcModeNone     = "none"

	// firelensConfigBindFormatFluentd and firelensConfigBindFormatFluentbit specify the format of the firelens
	// config file bind mount for fluentd and fluentbit firelens container respectively.
	// First placeholder is host data dir, second placeholder is taskID.
	firelensConfigBindFormatFluentd   = "%s/data/firelens/%s/config/fluent.conf:/fluentd/etc/fluent.conf"
	firelensConfigBindFormatFluentbit = "%s/data/firelens/%s/config/fluent.conf:/fluent-bit/etc/fluent-bit.conf"

	// firelensS3ConfigBindFormat specifies the format of the bind mount for the firelens config file downloaded from S3.
	// First placeholder is host data dir, second placeholder is taskID, third placeholder is the s3 config path inside
	// the firelens container.
	firelensS3ConfigBindFormat = "%s/data/firelens/%s/config/external.conf:%s"

	// firelensSocketBindFormat specifies the format for firelens container's socket directory bind mount.
	// First placeholder is host data dir, second placeholder is taskID.
	firelensSocketBindFormat = "%s/data/firelens/%s/socket/:/var/run/"
	// firelensDriverName is the log driver name for containers that want to use the firelens container to send logs.
	firelensDriverName = "awsfirelens"
	// FirelensLogDriverBufferLimitOption is the option for customers who want to specify the buffer limit size in FireLens.
	FirelensLogDriverBufferLimitOption = "log-driver-buffer-limit"

	// firelensConfigVarFmt specifies the format for firelens config variable name. The first placeholder
	// is option name. The second placeholder is the index of the container in the task's container list, appended
	// for the purpose of avoiding config vars from different containers within a task collide (note: can't append
	// container name here because it may contain hyphen which will break the config var resolution (see PR 2164 for
	// details), and can't append container ID either because we need the config var in PostUnmarshalTask, which is
	// before all the containers being created).
	firelensConfigVarFmt = "%s_%d"
	// firelensConfigVarPlaceholderFmtFluentd and firelensConfigVarPlaceholderFmtFluentbit specify the config var
	// placeholder format expected by fluentd and fluentbit respectively.
	firelensConfigVarPlaceholderFmtFluentd   = "\"#{ENV['%s']}\""
	firelensConfigVarPlaceholderFmtFluentbit = "${%s}"

	// awsExecutionEnvKey is the key of the env specifying the execution environment.
	awsExecutionEnvKey = "AWS_EXECUTION_ENV"
	// ec2ExecutionEnv specifies the ec2 execution environment.
	ec2ExecutionEnv = "AWS_ECS_EC2"

	// BridgeNetworkMode specifies bridge type mode for a task
	BridgeNetworkMode = "bridge"

	// AWSVPCNetworkMode specifies awsvpc type mode for a task
	AWSVPCNetworkMode = "awsvpc"

	// HostNetworkMode specifies host type mode for a task
	HostNetworkMode = "host"

	// disableIPv6SysctlKey specifies the setting that controls whether ipv6 is disabled.
	disableIPv6SysctlKey = "net.ipv6.conf.all.disable_ipv6"
	// sysctlValueOff specifies the value to use to turn off a sysctl setting.
	sysctlValueOff = "0"

	serviceConnectListenerPortMappingEnvVar = "APPNET_LISTENER_PORT_MAPPING"
	serviceConnectContainerMappingEnvVar    = "APPNET_CONTAINER_IP_MAPPING"
	// ServiceConnectAttachmentType specifies attachment type for service connect
	serviceConnectAttachmentType = "serviceconnectdetail"

	ipv6LoopbackAddress = "::1"
)

// TaskOverrides are the overrides applied to a task
type TaskOverrides struct{}

// Task is the internal representation of a task in the ECS agent
type Task struct {
	// Arn is the unique identifier for the task
	Arn string
	// id is the id section of the task ARN
	id string
	// Overrides are the overrides applied to a task
	Overrides TaskOverrides `json:"-"`
	// Family is the name of the task definition family
	Family string
	// Version is the version of the task definition
	Version string
	// ServiceName is the name of the service to which the task belongs.
	// It is empty if the task does not belong to any service.
	ServiceName string
	// Containers are the containers for the task
	Containers []*apicontainer.Container
	// Associations are the available associations for the task.
	Associations []Association `json:"associations"`
	// ResourcesMapUnsafe is the map of resource type to corresponding resources
	ResourcesMapUnsafe resourcetype.ResourcesMap `json:"resources"`
	// Volumes are the volumes for the task
	Volumes []TaskVolume `json:"volumes"`
	// CPU is a task-level limit for compute resources. A value of 1 means that
	// the task may access 100% of 1 vCPU on the instance
	CPU float64 `json:"Cpu,omitempty"`
	// Memory is a task-level limit for memory resources in bytes
	Memory int64 `json:"Memory,omitempty"`
	// DesiredStatusUnsafe represents the state where the task should go. Generally,
	// the desired status is informed by the ECS backend as a result of either
	// API calls made to ECS or decisions made by the ECS service scheduler.
	// The DesiredStatusUnsafe is almost always either apitaskstatus.TaskRunning or apitaskstatus.TaskStopped.
	// NOTE: Do not access DesiredStatusUnsafe directly.
	// Instead, use `UpdateStatus`, `UpdateDesiredStatus`, and `SetDesiredStatus`.
	// TODO DesiredStatusUnsafe should probably be private with appropriately written
	// setter/getter.  When this is done, we need to ensure that the UnmarshalJSON
	// is handled properly so that the state storage continues to work.
	DesiredStatusUnsafe apitaskstatus.TaskStatus `json:"DesiredStatus"`

	// KnownStatusUnsafe represents the state where the task is.  This is generally
	// the minimum of equivalent status types for the containers in the task;
	// if one container is at ContainerRunning and another is at ContainerPulled,
	// the task KnownStatusUnsafe would be TaskPulled.
	// NOTE: Do not access KnownStatusUnsafe directly.  Instead, use `UpdateStatus`,
	// and `GetKnownStatus`.
	// TODO KnownStatusUnsafe should probably be private with appropriately written
	// setter/getter.  When this is done, we need to ensure that the UnmarshalJSON
	// is handled properly so that the state storage continues to work.
	KnownStatusUnsafe apitaskstatus.TaskStatus `json:"KnownStatus"`
	// KnownStatusTimeUnsafe captures the time when the KnownStatusUnsafe was last updated.
	// NOTE: Do not access KnownStatusTime directly, instead use `GetKnownStatusTime`.
	KnownStatusTimeUnsafe time.Time `json:"KnownTime"`

	// PullStartedAtUnsafe is the timestamp when the task start pulling the first container,
	// it won't be set if the pull never happens
	PullStartedAtUnsafe time.Time `json:"PullStartedAt"`
	// PullStoppedAtUnsafe is the timestamp when the task finished pulling the last container,
	// it won't be set if the pull never happens
	PullStoppedAtUnsafe time.Time `json:"PullStoppedAt"`
	// ExecutionStoppedAtUnsafe is the timestamp when the task desired status moved to stopped,
	// which is when any of the essential containers stopped
	ExecutionStoppedAtUnsafe time.Time `json:"ExecutionStoppedAt"`

	// SentStatusUnsafe represents the last KnownStatusUnsafe that was sent to the ECS SubmitTaskStateChange API.
	// TODO SentStatusUnsafe should probably be private with appropriately written
	// setter/getter.  When this is done, we need to ensure that the UnmarshalJSON
	// is handled properly so that the state storage continues to work.
	SentStatusUnsafe apitaskstatus.TaskStatus `json:"SentStatus"`

	// ExecutionCredentialsID is the ID of credentials that are used by agent to
	// perform some action at the task level, such as pulling image from ECR
	ExecutionCredentialsID string `json:"executionCredentialsID"`

	// credentialsID is used to set the CredentialsId field for the
	// IAMRoleCredentials object associated with the task. This id can be
	// used to look up the credentials for task in the credentials manager
	credentialsID                string
	credentialsRelativeURIUnsafe string

	// ENIs is the list of Elastic Network Interfaces assigned to this task. The
	// TaskENIs type is helpful when decoding state files which might have stored
	// ENIs as a single ENI object instead of a list.
	ENIs TaskENIs `json:"ENI"`

	// AppMesh is the service mesh specified by the task
	AppMesh *nlappmesh.AppMesh

	// MemoryCPULimitsEnabled to determine if task supports CPU, memory limits
	MemoryCPULimitsEnabled bool `json:"MemoryCPULimitsEnabled,omitempty"`

	// PlatformFields consists of fields specific to linux/windows for a task
	PlatformFields PlatformFields `json:"PlatformFields,omitempty"`

	// terminalReason should be used when we explicitly move a task to stopped.
	// This ensures the task object carries some context for why it was explicitly stopped.
	terminalReason     string
	terminalReasonOnce sync.Once

	// PIDMode is used to determine how PID namespaces are organized between
	// containers of the Task
	PIDMode string `json:"PidMode,omitempty"`

	// IPCMode is used to determine how IPC resources should be shared among
	// containers of the Task
	IPCMode string `json:"IpcMode,omitempty"`

	// NvidiaRuntime is the runtime to pass Nvidia GPU devices to containers
	NvidiaRuntime string `json:"NvidiaRuntime,omitempty"`

	// LocalIPAddressUnsafe stores the local IP address allocated to the bridge that connects the task network
	// namespace and the host network namespace, for tasks in awsvpc network mode (tasks in other network mode won't
	// have a value for this). This field should be accessed via GetLocalIPAddress and SetLocalIPAddress.
	LocalIPAddressUnsafe string `json:"LocalIPAddress,omitempty"`

	// LaunchType is the launch type of this task.
	LaunchType string `json:"LaunchType,omitempty"`

	// lock is for protecting all fields in the task struct
	lock sync.RWMutex

	// setIdOnce is used to set the value of this task's id only the first time GetID is invoked
	setIdOnce sync.Once

	ServiceConnectConfig *serviceconnect.Config `json:"ServiceConnectConfig,omitempty"`

	ServiceConnectConnectionDrainingUnsafe bool `json:"ServiceConnectConnectionDraining,omitempty"`

	NetworkMode string `json:"NetworkMode,omitempty"`

	IsInternal bool `json:"IsInternal,omitempty"`

	NetworkNamespace string `json:"NetworkNamespace,omitempty"`

	EnableFaultInjection bool `json:"enableFaultInjection,omitempty"`

	// DefaultIfname is used to reference the default network interface name on the task network namespace
	// For AWSVPC mode, it can be eth0 which corresponds to the interface name on the task ENI
	// For Host mode, it can vary based on the hardware/network config on the host instance (e.g. eth0, ens5, etc.) and will need to be obtained on the host.
	// For all other network modes (i.e. bridge, none, etc.), DefaultIfname is currently not being initialized/set. In order to use this task field for these
	// network modes, changes will need to be made in the corresponding task provisioning workflows.
	DefaultIfname string `json:"DefaultIfname,omitempty"`
}

// TaskFromACS translates ecsacs.Task to apitask.Task by first marshaling the received
// ecsacs.Task to json and unmarshalling it as apitask.Task
func TaskFromACS(acsTask *ecsacs.Task, envelope *ecsacs.PayloadMessage) (*Task, error) {
	data, err := json.Marshal(acsTask)
	if err != nil {
		return nil, err
	}
	task := &Task{}
	if err := json.Unmarshal(data, task); err != nil {
		return nil, err
	}

	// Set the EnableFaultInjection field if present
	task.EnableFaultInjection = aws.ToBool(acsTask.EnableFaultInjection)

	// Overrides the container command if it's set
	for _, container := range task.Containers {
		if (container.Overrides != apicontainer.ContainerOverrides{}) && container.Overrides.Command != nil {
			container.Command = *container.Overrides.Command
		}
		container.TransitionDependenciesMap = make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet)
	}

	//initialize resources map for task
	task.ResourcesMapUnsafe = make(map[string][]taskresource.TaskResource)

	task.initNetworkMode(acsTask.NetworkMode)

	// extract and validate attachments
	if err := handleTaskAttachments(acsTask, task); err != nil {
		return nil, err
	}

	return task, nil
}

func (task *Task) RemoveVolume(index int) {
	task.lock.Lock()
	defer task.lock.Unlock()
	task.removeVolumeUnsafe(index)
}

func (task *Task) removeVolumeUnsafe(index int) {
	if index < 0 || index >= len(task.Volumes) {
		return
	}
	out := make([]TaskVolume, 0)
	out = append(out, task.Volumes[:index]...)
	out = append(out, task.Volumes[index+1:]...)
	task.Volumes = out
}

func (task *Task) initializeVolumes(cfg *config.Config, dockerClient dockerapi.DockerClient, ctx context.Context) error {
	// TODO: Have EBS volumes use the DockerVolumeConfig to create the mountpoint
	err := task.initializeDockerLocalVolumes(dockerClient, ctx)
	if err != nil {
		return apierrors.NewResourceInitError(task.Arn, err)
	}
	err = task.initializeDockerVolumes(cfg.SharedVolumeMatchFullConfig.Enabled(), dockerClient, ctx)
	if err != nil {
		return apierrors.NewResourceInitError(task.Arn, err)
	}
	err = task.initializeEFSVolumes(cfg, dockerClient, ctx)
	if err != nil {
		return apierrors.NewResourceInitError(task.Arn, err)
	}
	return nil
}

// PostUnmarshalTask is run after a task has been unmarshalled, but before it has been
// run. It is possible it will be subsequently called after that and should be
// able to handle such an occurrence appropriately (e.g. behave idempotently).
func (task *Task) PostUnmarshalTask(cfg *config.Config,
	credentialsManager credentials.Manager, resourceFields *taskresource.ResourceFields,
	dockerClient dockerapi.DockerClient, ctx context.Context, options ...Option) error {

	task.adjustForPlatform(cfg)

	// Initialize cgroup resource spec definition for later cgroup resource creation.
	// This sets up the cgroup spec for cpu, memory, and pids limits for the task.
	// Actual cgroup creation happens later.
	if err := task.initializeCgroupResourceSpec(cfg.CgroupPath, cfg.CgroupCPUPeriod, cfg.TaskPidsLimit, resourceFields); err != nil {
		logger.Error("Could not initialize resource", logger.Fields{
			field.TaskID: task.GetID(),
			field.Error:  err,
		})
		return apierrors.NewResourceInitError(task.Arn, err)
	}

	if err := task.initServiceConnectResources(); err != nil {
		logger.Error("Could not initialize Service Connect resources", logger.Fields{
			field.TaskID: task.GetID(),
			field.Error:  err,
		})
		return apierrors.NewResourceInitError(task.Arn, err)
	}

	if err := task.initializeContainerOrdering(); err != nil {
		logger.Error("Could not initialize dependency for container", logger.Fields{
			field.TaskID: task.GetID(),
			field.Error:  err,
		})
		return apierrors.NewResourceInitError(task.Arn, err)
	}

	task.initSecretResources(cfg, credentialsManager, resourceFields)

	task.initializeCredentialsEndpoint(credentialsManager)

	// NOTE: initializeVolumes needs to be after initializeCredentialsEndpoint, because EFS volume might
	// need the credentials endpoint constructed by it.
	if err := task.initializeVolumes(cfg, dockerClient, ctx); err != nil {
		return err
	}

	if err := task.addGPUResource(cfg); err != nil {
		logger.Error("Could not initialize GPU associations", logger.Fields{
			field.TaskID: task.GetID(),
			field.Error:  err,
		})
		return apierrors.NewResourceInitError(task.Arn, err)
	}

	task.initializeContainersV3MetadataEndpoint(utils.NewDynamicUUIDProvider())
	task.initializeContainersV4MetadataEndpoint(utils.NewDynamicUUIDProvider())
	task.initializeContainersV1AgentAPIEndpoint(utils.NewDynamicUUIDProvider())
	if err := task.addNetworkResourceProvisioningDependency(cfg); err != nil {
		logger.Error("Could not provision network resource", logger.Fields{
			field.TaskID: task.GetID(),
			field.Error:  err,
		})
		return apierrors.NewResourceInitError(task.Arn, err)
	}
	// Adds necessary Pause containers for sharing PID or IPC namespaces
	task.addNamespaceSharingProvisioningDependency(cfg)

	if err := task.applyFirelensSetup(cfg, resourceFields, credentialsManager); err != nil {
		return err
	}

	if task.requiresAnyCredentialSpecResource() {
		if err := task.initializeCredentialSpecResource(cfg, credentialsManager, resourceFields); err != nil {
			logger.Error("Could not initialize credentialspec resource", logger.Fields{
				field.TaskID: task.GetID(),
				field.Error:  err,
			})
			return apierrors.NewResourceInitError(task.Arn, err)
		}
	}

	if err := task.initializeEnvfilesResource(cfg, credentialsManager); err != nil {
		logger.Error("Could not initialize environment files resource", logger.Fields{
			field.TaskID: task.GetID(),
			field.Error:  err,
		})
		return apierrors.NewResourceInitError(task.Arn, err)
	}
	task.populateTaskARN()

	// fsxWindowsFileserver is the product type -- it is technically "agnostic" ie it should apply to both Windows and Linux tasks
	if task.requiresFSxWindowsFileServerResource() {
		if err := task.initializeFSxWindowsFileServerResource(cfg, credentialsManager, resourceFields); err != nil {
			logger.Error("Could not initialize FSx for Windows File Server resource", logger.Fields{
				field.TaskID: task.GetID(),
				field.Error:  err,
			})
			return apierrors.NewResourceInitError(task.Arn, err)
		}
	}

	task.initRestartTrackers()

	for _, opt := range options {
		if err := opt(task); err != nil {
			logger.Error("Could not apply task option", logger.Fields{
				field.TaskID: task.GetID(),
				field.Error:  err,
			})
			return err
		}
	}
	return nil
}

// initializeCredentialSpecResource builds the resource dependency map for the credentialspec resource
func (task *Task) initializeCredentialSpecResource(config *config.Config, credentialsManager credentials.Manager,
	resourceFields *taskresource.ResourceFields) error {
	credspecContainerMapping := task.GetAllCredentialSpecRequirements()
	credentialspecResource, err := credentialspec.NewCredentialSpecResource(task.Arn, config.AWSRegion, task.ExecutionCredentialsID,
		credentialsManager, resourceFields.SSMClientCreator, resourceFields.S3ClientCreator, resourceFields.ASMClientCreator, credspecContainerMapping, config.InstanceIPCompatibility)
	if err != nil {
		return err
	}

	task.AddResource(credentialspec.ResourceName, credentialspecResource)

	// for every container that needs credential spec vending, it needs to wait for all credential spec resources
	for _, container := range task.Containers {
		if container.RequiresAnyCredentialSpec() {
			container.BuildResourceDependency(credentialspecResource.GetName(),
				resourcestatus.ResourceStatus(credentialspec.CredentialSpecCreated),
				apicontainerstatus.ContainerCreated)
		}
	}

	return nil
}

// initNetworkMode initializes/infers the network mode for the task and assigns the result to this task's NetworkMode field.
// ACS is streaming down this value with task payload. In case of docker bridge mode task, this value might be left empty
// as it's the default task network mode.
func (task *Task) initNetworkMode(acsTaskNetworkMode *string) {
	switch aws.ToString(acsTaskNetworkMode) {
	case AWSVPCNetworkMode:
		task.NetworkMode = AWSVPCNetworkMode
	case HostNetworkMode:
		task.NetworkMode = HostNetworkMode
	case BridgeNetworkMode, "":
		task.NetworkMode = BridgeNetworkMode
	case networkModeNone:
		task.NetworkMode = networkModeNone
	default:
		logger.Warn("Unmapped task network mode", logger.Fields{
			field.TaskID:      task.GetID(),
			field.NetworkMode: aws.ToString(acsTaskNetworkMode),
		})
	}
}

// initRestartTrackers initializes the restart policy tracker for each container
// that has a restart policy configured and enabled.
func (task *Task) initRestartTrackers() {
	for _, c := range task.Containers {
		if c.RestartPolicyEnabled() {
			c.RestartTracker = restart.NewRestartTracker(*c.RestartPolicy)
		}
	}
}

func (task *Task) initServiceConnectResources() error {
	// TODO [SC]: ServiceConnectConfig will come from ACS. Adding this here for dev/testing purposes only Remove when
	// ACS model is integrated
	if task.ServiceConnectConfig == nil {
		task.ServiceConnectConfig = &serviceconnect.Config{
			ContainerName: "service-connect",
		}
	}
	if task.IsServiceConnectEnabled() {
		// TODO [SC]: initDummyServiceConnectConfig is for dev testing only, remove it when final SC model from ACS is in place
		task.initDummyServiceConnectConfig()
		if err := task.initServiceConnectEphemeralPorts(); err != nil {
			return err
		}
	}
	return nil
}

// TODO [SC]: This is for dev testing only, remove it when final SC model from ACS is in place
func (task *Task) initDummyServiceConnectConfig() {
	scContainer := task.GetServiceConnectContainer()
	if _, ok := scContainer.Environment["SC_CONFIG"]; !ok {
		// no SC_CONFIG :(
		return
	}
	if err := json.Unmarshal([]byte(scContainer.Environment["SC_CONFIG"]), task.ServiceConnectConfig); err != nil {
		logger.Error("Error parsing SC_CONFIG", logger.Fields{
			field.Error: err,
		})
		return
	}
}

func (task *Task) initServiceConnectEphemeralPorts() error {
	var utilizedPorts []uint16
	// First determine how many ephemeral ports we need
	var numEphemeralPortsNeeded int
	for _, ic := range task.ServiceConnectConfig.IngressConfig {
		if ic.ListenerPort == 0 { // This means listener port was not sent to us by ACS, signaling the port needs to be ephemeral
			numEphemeralPortsNeeded++
		} else {
			utilizedPorts = append(utilizedPorts, ic.ListenerPort)
		}
	}

	// Presently, SC egress port is always ephemeral, but adding this for future-proofing
	if task.ServiceConnectConfig.EgressConfig != nil {
		if task.ServiceConnectConfig.EgressConfig.ListenerPort == 0 {
			numEphemeralPortsNeeded++
		} else {
			utilizedPorts = append(utilizedPorts, task.ServiceConnectConfig.EgressConfig.ListenerPort)
		}
	}

	// Get all exposed ports in the task so that the ephemeral port generator doesn't take those into account in order
	// to avoid port conflicts.
	for _, c := range task.Containers {
		for _, p := range c.Ports {
			utilizedPorts = append(utilizedPorts, p.ContainerPort)
		}
	}

	ephemeralPorts, err := utils.GenerateEphemeralPortNumbers(numEphemeralPortsNeeded, utilizedPorts)
	if err != nil {
		return fmt.Errorf("error initializing ports for Service Connect: %w", err)
	}

	// Assign ephemeral ports
	portMapping := make(map[string]uint16)
	var curEphemeralIndex int
	for i, ic := range task.ServiceConnectConfig.IngressConfig {
		if ic.ListenerPort == 0 {
			portMapping[ic.ListenerName] = ephemeralPorts[curEphemeralIndex]
			task.ServiceConnectConfig.IngressConfig[i].ListenerPort = ephemeralPorts[curEphemeralIndex]
			curEphemeralIndex++
		}
	}

	if task.ServiceConnectConfig.EgressConfig != nil && task.ServiceConnectConfig.EgressConfig.ListenerPort == 0 {
		portMapping[task.ServiceConnectConfig.EgressConfig.ListenerName] = ephemeralPorts[curEphemeralIndex]
		task.ServiceConnectConfig.EgressConfig.ListenerPort = ephemeralPorts[curEphemeralIndex]
	}

	// Add the APPNET_LISTENER_PORT_MAPPING env var for listeners that require it
	envVars := make(map[string]string)
	portMappingJson, err := json.Marshal(portMapping)
	if err != nil {
		return fmt.Errorf("error injecting required env vars to Service Connect container: %w", err)
	}
	envVars[serviceConnectListenerPortMappingEnvVar] = string(portMappingJson)
	task.GetServiceConnectContainer().MergeEnvironmentVariables(envVars)
	return nil
}

// populateTaskARN populates the arn of the task to the containers.
func (task *Task) populateTaskARN() {
	for _, c := range task.Containers {
		c.SetTaskARN(task.Arn)
	}
}

func (task *Task) initSecretResources(cfg *config.Config, credentialsManager credentials.Manager,
	resourceFields *taskresource.ResourceFields) {
	if task.requiresASMDockerAuthData() {
		task.initializeASMAuthResource(credentialsManager, resourceFields)
	}

	if task.requiresSSMSecret() {
		task.initializeSSMSecretResource(cfg, credentialsManager, resourceFields)
	}

	if task.requiresASMSecret() {
		task.initializeASMSecretResource(credentialsManager, resourceFields)
	}
}

func (task *Task) applyFirelensSetup(cfg *config.Config, resourceFields *taskresource.ResourceFields,
	credentialsManager credentials.Manager) error {
	firelensContainer := task.GetFirelensContainer()
	if firelensContainer != nil {
		if err := task.initializeFirelensResource(cfg, resourceFields, firelensContainer, credentialsManager); err != nil {
			return apierrors.NewResourceInitError(task.Arn, err)
		}
		if err := task.addFirelensContainerDependency(); err != nil {
			return errors.New("unable to add firelens container dependency")
		}
	}

	return nil
}

func (task *Task) addGPUResource(cfg *config.Config) error {
	if cfg.GPUSupportEnabled {
		for _, association := range task.Associations {
			// One GPU can be associated with only one container
			// That is why validating if association.Containers is of length 1
			if association.Type == GPUAssociationType {
				if len(association.Containers) != 1 {
					return fmt.Errorf("could not associate multiple containers to GPU %s", association.Name)
				}

				container, ok := task.ContainerByName(association.Containers[0])
				if !ok {
					return fmt.Errorf("could not find container with name %s for associating GPU %s",
						association.Containers[0], association.Name)
				}
				container.GPUIDs = append(container.GPUIDs, association.Name)
			}
		}
		// For external instances, GPU IDs are handled by resources struct
		// For internal instances, GPU IDs are handled by env var
		if !cfg.External.Enabled() {
			task.populateGPUEnvironmentVariables()
			task.NvidiaRuntime = cfg.NvidiaRuntime
		}
	}
	return nil
}

func (task *Task) isGPUEnabled() bool {
	for _, association := range task.Associations {
		if association.Type == GPUAssociationType {
			return true
		}
	}
	return false
}

func (task *Task) populateGPUEnvironmentVariables() {
	for _, container := range task.Containers {
		if len(container.GPUIDs) > 0 {
			gpuList := strings.Join(container.GPUIDs, ",")
			envVars := make(map[string]string)
			envVars[NvidiaVisibleDevicesEnvVar] = gpuList
			container.MergeEnvironmentVariables(envVars)
		}
	}
}

func (task *Task) shouldRequireNvidiaRuntime(container *apicontainer.Container) bool {
	_, ok := container.Environment[NvidiaVisibleDevicesEnvVar]
	return ok
}

func (task *Task) initializeDockerLocalVolumes(dockerClient dockerapi.DockerClient, ctx context.Context) error {
	var requiredLocalVolumes []string
	for _, container := range task.Containers {
		for _, mountPoint := range container.MountPoints {
			vol, ok := task.HostVolumeByName(mountPoint.SourceVolume)
			if !ok {
				continue
			}
			if localVolume, ok := vol.(*taskresourcevolume.LocalDockerVolume); ok {
				localVolume.HostPath = task.volumeName(mountPoint.SourceVolume)
				container.BuildResourceDependency(mountPoint.SourceVolume,
					resourcestatus.ResourceStatus(taskresourcevolume.VolumeCreated),
					apicontainerstatus.ContainerPulled)
				requiredLocalVolumes = append(requiredLocalVolumes, mountPoint.SourceVolume)
			}
		}
	}

	if len(requiredLocalVolumes) == 0 {
		// No need to create the auxiliary local driver volumes
		return nil
	}

	// if we have required local volumes, create one with default local drive
	for _, volumeName := range requiredLocalVolumes {
		vol, _ := task.HostVolumeByName(volumeName)
		// BUG(samuelkarp) On Windows, volumes with names that differ only by case will collide
		scope := taskresourcevolume.TaskScope
		localVolume, err := taskresourcevolume.NewVolumeResource(ctx, volumeName, HostVolumeType,
			vol.Source(), scope, false,
			taskresourcevolume.DockerLocalVolumeDriver,
			make(map[string]string), make(map[string]string), dockerClient)

		if err != nil {
			return err
		}

		task.AddResource(resourcetype.DockerVolumeKey, localVolume)
	}
	return nil
}

func (task *Task) volumeName(name string) string {
	return "ecs-" + task.Family + "-" + task.Version + "-" + name + "-" + utils.RandHex()
}

// initializeDockerVolumes checks the volume resource in the task to determine if the agent
// should create the volume before creating the container
func (task *Task) initializeDockerVolumes(sharedVolumeMatchFullConfig bool, dockerClient dockerapi.DockerClient, ctx context.Context) error {
	for i, vol := range task.Volumes {
		// No need to do this for non-docker volume, eg: host bind/empty volume
		if vol.Type != DockerVolumeType {
			continue
		}

		dockerVolume, ok := vol.Volume.(*taskresourcevolume.DockerVolumeConfig)
		if !ok {
			return errors.New("task volume: volume configuration does not match the type 'docker'")
		}
		// Agent needs to create task-scoped volume
		if dockerVolume.Scope == taskresourcevolume.TaskScope {
			if err := task.addTaskScopedVolumes(ctx, dockerClient, &task.Volumes[i]); err != nil {
				return err
			}
		} else {
			// Agent needs to create shared volume if that's auto provisioned
			if err := task.addSharedVolumes(sharedVolumeMatchFullConfig, ctx, dockerClient, &task.Volumes[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// initializeEFSVolumes inspects the volume definitions in the task definition.
// If it finds EFS volumes in the task definition, then it converts it to a docker
// volume definition.
func (task *Task) initializeEFSVolumes(cfg *config.Config, dockerClient dockerapi.DockerClient, ctx context.Context) error {
	for i, vol := range task.Volumes {
		// No need to do this for non-efs volume, eg: host bind/empty volume
		if vol.Type != EFSVolumeType {
			continue
		}

		efsvol, ok := vol.Volume.(*taskresourcevolume.EFSVolumeConfig)
		if !ok {
			return errors.New("task volume: volume configuration does not match the type 'efs'")
		}

		err := task.addEFSVolumes(ctx, cfg, dockerClient, &task.Volumes[i], efsvol)
		if err != nil {
			return err
		}
	}
	return nil
}

// addEFSVolumes converts the EFS task definition into an internal docker 'local' volume
// mounted with NFS struct and updates container dependency
func (task *Task) addEFSVolumes(
	ctx context.Context,
	cfg *config.Config,
	dockerClient dockerapi.DockerClient,
	vol *TaskVolume,
	efsvol *taskresourcevolume.EFSVolumeConfig,
) error {
	driverOpts := taskresourcevolume.GetDriverOptions(cfg, efsvol, task.GetCredentialsRelativeURI())
	driverName := getEFSVolumeDriverName(cfg)
	volumeResource, err := taskresourcevolume.NewVolumeResource(
		ctx,
		vol.Name,
		EFSVolumeType,
		task.volumeName(vol.Name),
		"task",
		false,
		driverName,
		driverOpts,
		map[string]string{},
		dockerClient,
	)
	if err != nil {
		return err
	}

	vol.Volume = &volumeResource.VolumeConfig
	task.AddResource(resourcetype.DockerVolumeKey, volumeResource)
	task.updateContainerVolumeDependency(vol.Name)
	return nil
}

// addTaskScopedVolumes adds the task scoped volume into task resources and updates container dependency
func (task *Task) addTaskScopedVolumes(ctx context.Context, dockerClient dockerapi.DockerClient,
	vol *TaskVolume) error {

	volumeConfig := vol.Volume.(*taskresourcevolume.DockerVolumeConfig)
	volumeResource, err := taskresourcevolume.NewVolumeResource(
		ctx,
		vol.Name,
		DockerVolumeType,
		task.volumeName(vol.Name),
		volumeConfig.Scope, volumeConfig.Autoprovision,
		volumeConfig.Driver, volumeConfig.DriverOpts,
		volumeConfig.Labels, dockerClient)
	if err != nil {
		return err
	}

	vol.Volume = &volumeResource.VolumeConfig
	task.AddResource(resourcetype.DockerVolumeKey, volumeResource)
	task.updateContainerVolumeDependency(vol.Name)
	return nil
}

// addSharedVolumes adds shared volume into task resources and updates container dependency
func (task *Task) addSharedVolumes(SharedVolumeMatchFullConfig bool, ctx context.Context, dockerClient dockerapi.DockerClient,
	vol *TaskVolume) error {

	volumeConfig := vol.Volume.(*taskresourcevolume.DockerVolumeConfig)
	volumeConfig.DockerVolumeName = vol.Name
	// if autoprovision == true, we will auto-provision the volume if it does not exist already
	// else the named volume must exist
	if !volumeConfig.Autoprovision {
		volumeMetadata := dockerClient.InspectVolume(ctx, vol.Name, dockerclient.InspectVolumeTimeout)
		if volumeMetadata.Error != nil {
			return errors.Wrapf(volumeMetadata.Error, "initialize volume: volume detection failed, volume '%s' does not exist and autoprovision is set to false", vol.Name)
		}
		return nil
	}

	// at this point we know autoprovision = true
	// check if the volume configuration matches the one exists on the instance
	volumeMetadata := dockerClient.InspectVolume(ctx, volumeConfig.DockerVolumeName, dockerclient.InspectVolumeTimeout)
	if volumeMetadata.Error != nil {
		// Inspect the volume timed out, fail the task
		if _, ok := volumeMetadata.Error.(*dockerapi.DockerTimeoutError); ok {
			return volumeMetadata.Error
		}
		logger.Error("Failed to initialize non-autoprovisioned volume", logger.Fields{
			field.TaskID: task.GetID(),
			field.Volume: vol.Name,
			field.Error:  volumeMetadata.Error,
		})
		// this resource should be created by agent
		volumeResource, err := taskresourcevolume.NewVolumeResource(
			ctx,
			vol.Name,
			DockerVolumeType,
			vol.Name,
			volumeConfig.Scope, volumeConfig.Autoprovision,
			volumeConfig.Driver, volumeConfig.DriverOpts,
			volumeConfig.Labels, dockerClient)
		if err != nil {
			return err
		}

		task.AddResource(resourcetype.DockerVolumeKey, volumeResource)
		task.updateContainerVolumeDependency(vol.Name)
		return nil
	}

	logger.Debug("Volume already exists", logger.Fields{
		field.TaskID: task.GetID(),
		field.Volume: volumeConfig.DockerVolumeName,
	})
	if !SharedVolumeMatchFullConfig {
		logger.Info("ECS_SHARED_VOLUME_MATCH_FULL_CONFIG is set to false and volume was found", logger.Fields{
			field.TaskID: task.GetID(),
			field.Volume: volumeConfig.DockerVolumeName,
		})
		return nil
	}

	// validate all the volume metadata fields match to the configuration
	if len(volumeMetadata.DockerVolume.Labels) == 0 && len(volumeMetadata.DockerVolume.Labels) == len(volumeConfig.Labels) {
		logger.Info("Volume labels are both empty or null", logger.Fields{
			field.TaskID: task.GetID(),
			field.Volume: volumeConfig.DockerVolumeName,
		})
	} else if !reflect.DeepEqual(volumeMetadata.DockerVolume.Labels, volumeConfig.Labels) {
		return errors.Errorf("intialize volume: non-autoprovisioned volume does not match existing volume labels: existing: %v, expected: %v",
			volumeMetadata.DockerVolume.Labels, volumeConfig.Labels)
	}

	if len(volumeMetadata.DockerVolume.Options) == 0 && len(volumeMetadata.DockerVolume.Options) == len(volumeConfig.DriverOpts) {
		logger.Info("Volume driver options are both empty or null", logger.Fields{
			field.TaskID: task.GetID(),
			field.Volume: volumeConfig.DockerVolumeName,
		})
	} else if !reflect.DeepEqual(volumeMetadata.DockerVolume.Options, volumeConfig.DriverOpts) {
		return errors.Errorf("initialize volume: non-autoprovisioned volume does not match existing volume options: existing: %v, expected: %v",
			volumeMetadata.DockerVolume.Options, volumeConfig.DriverOpts)
	}
	// Right now we are not adding shared, autoprovision = true volume to task as resource if it already exists (i.e. when this task didn't create the volume).
	// if we need to change that, make a call to task.AddResource here.
	return nil
}

// updateContainerVolumeDependency adds the volume resource to container dependency
func (task *Task) updateContainerVolumeDependency(name string) {
	// Find all the container that depends on the volume
	for _, container := range task.Containers {
		for _, mountpoint := range container.MountPoints {
			if mountpoint.SourceVolume == name {
				container.BuildResourceDependency(name,
					resourcestatus.ResourceCreated,
					apicontainerstatus.ContainerPulled)
			}
		}
	}
}

// initializeCredentialsEndpoint sets the credentials endpoint for all containers in a task if needed.
func (task *Task) initializeCredentialsEndpoint(credentialsManager credentials.Manager) {
	id := task.GetCredentialsID()
	if id == "" {
		// No credentials set for the task. Do not inject the endpoint environment variable.
		return
	}
	taskCredentials, ok := credentialsManager.GetTaskCredentials(id)
	if !ok {
		// Task has credentials id set, but credentials manager is unaware of
		// the id. This should never happen as the payload handler sets
		// credentialsId for the task after adding credentials to the
		// credentials manager
		logger.Error("Unable to get credentials for task", logger.Fields{
			field.TaskID: task.GetID(),
		})
		return
	}

	credentialsEndpointRelativeURI := taskCredentials.IAMRoleCredentials.GenerateCredentialsEndpointRelativeURI()
	for _, container := range task.Containers {
		// container.Environment map would not be initialized if there are
		// no environment variables to be set or overridden in the container
		// config. Check if that's the case and initialize if needed
		if container.Environment == nil {
			container.Environment = make(map[string]string)
		}
		container.Environment[awsSDKCredentialsRelativeURIPathEnvironmentVariableName] = credentialsEndpointRelativeURI
	}

	task.SetCredentialsRelativeURI(credentialsEndpointRelativeURI)
}

// initializeContainersV3MetadataEndpoint generates a v3 endpoint id for each container, constructs the
// v3 metadata endpoint, and injects it as an environment variable
func (task *Task) initializeContainersV3MetadataEndpoint(uuidProvider utils.UUIDProvider) {
	task.initializeV3EndpointIDForAllContainers(uuidProvider)
	for _, container := range task.Containers {
		container.InjectV3MetadataEndpoint()
	}
}

// initializeContainersV4MetadataEndpoint generates a v4 endpoint id which we reuse the v3 container id
// (they are the same) for each container, constructs the v4 metadata endpoint,
// and injects it as an environment variable
func (task *Task) initializeContainersV4MetadataEndpoint(uuidProvider utils.UUIDProvider) {
	task.initializeV3EndpointIDForAllContainers(uuidProvider)
	for _, container := range task.Containers {
		container.InjectV4MetadataEndpoint()
	}
}

// For each container of the task, initializeContainersV1AgentAPIEndpoint initializes
// its V3EndpointID (if not already initialized), and injects V1 Agent API Endpoint
// into the container.
func (task *Task) initializeContainersV1AgentAPIEndpoint(uuidProvider utils.UUIDProvider) {
	task.initializeV3EndpointIDForAllContainers(uuidProvider)
	for _, container := range task.Containers {
		container.InjectV1AgentAPIEndpoint()
	}
}

// Initializes V3EndpointID for all containers of the task if not already initialized.
// The ID is generated using the passed in UUIDProvider.
func (task *Task) initializeV3EndpointIDForAllContainers(uuidProvider utils.UUIDProvider) {
	for _, container := range task.Containers {
		v3EndpointID := container.GetV3EndpointID()
		if v3EndpointID == "" { // if container's v3 endpoint has not been set
			container.SetV3EndpointID(uuidProvider.New())
		}
	}
}

// requiresASMDockerAuthData returns true if atleast one container in the task
// needs to retrieve private registry authentication data from ASM
func (task *Task) requiresASMDockerAuthData() bool {
	for _, container := range task.Containers {
		if container.ShouldPullWithASMAuth() {
			return true
		}
	}
	return false
}

// initializeASMAuthResource builds the resource dependency map for the ASM auth resource
func (task *Task) initializeASMAuthResource(credentialsManager credentials.Manager,
	resourceFields *taskresource.ResourceFields) {
	asmAuthResource := asmauth.NewASMAuthResource(task.Arn, task.getAllASMAuthDataRequirements(),
		task.ExecutionCredentialsID, credentialsManager, resourceFields.ASMClientCreator)
	task.AddResource(asmauth.ResourceName, asmAuthResource)
	for _, container := range task.Containers {
		if container.ShouldPullWithASMAuth() {
			container.BuildResourceDependency(asmAuthResource.GetName(),
				resourcestatus.ResourceStatus(asmauth.ASMAuthStatusCreated),
				apicontainerstatus.ContainerManifestPulled)
			container.BuildResourceDependency(asmAuthResource.GetName(),
				resourcestatus.ResourceStatus(asmauth.ASMAuthStatusCreated),
				apicontainerstatus.ContainerPulled)
		}
	}
}

func (task *Task) getAllASMAuthDataRequirements() []*apicontainer.ASMAuthData {
	var reqs []*apicontainer.ASMAuthData
	for _, container := range task.Containers {
		if container.ShouldPullWithASMAuth() {
			reqs = append(reqs, container.RegistryAuthentication.ASMAuthData)
		}
	}
	return reqs
}

// requiresSSMSecret returns true if at least one container in the task
// needs to retrieve secret from SSM parameter
func (task *Task) requiresSSMSecret() bool {
	for _, container := range task.Containers {
		if container.ShouldCreateWithSSMSecret() {
			return true
		}
	}
	return false
}

// initializeSSMSecretResource builds the resource dependency map for the SSM ssmsecret resource
func (task *Task) initializeSSMSecretResource(cfg *config.Config,
	credentialsManager credentials.Manager,
	resourceFields *taskresource.ResourceFields) {
	ssmSecretResource := ssmsecret.NewSSMSecretResource(task.Arn, task.getAllSSMSecretRequirements(),
		task.ExecutionCredentialsID, credentialsManager, resourceFields.SSMClientCreator, cfg.InstanceIPCompatibility)
	task.AddResource(ssmsecret.ResourceName, ssmSecretResource)

	// for every container that needs ssm secret vending as env, it needs to wait all secrets got retrieved
	for _, container := range task.Containers {
		if container.ShouldCreateWithSSMSecret() {
			container.BuildResourceDependency(ssmSecretResource.GetName(),
				resourcestatus.ResourceStatus(ssmsecret.SSMSecretCreated),
				apicontainerstatus.ContainerCreated)
		}

		// Firelens container needs to depend on secret if other containers use secret log options.
		if container.GetFirelensConfig() != nil && task.firelensDependsOnSecretResource(apicontainer.SecretProviderSSM) {
			container.BuildResourceDependency(ssmSecretResource.GetName(),
				resourcestatus.ResourceStatus(ssmsecret.SSMSecretCreated),
				apicontainerstatus.ContainerCreated)
		}
	}
}

// firelensDependsOnSecret checks whether the firelens container needs to depend on a secret resource of
// a certain provider type.
func (task *Task) firelensDependsOnSecretResource(secretProvider string) bool {
	isLogDriverSecretWithGivenProvider := func(s apicontainer.Secret) bool {
		return s.Provider == secretProvider && s.Target == apicontainer.SecretTargetLogDriver
	}
	for _, container := range task.Containers {
		if container.GetLogDriver() == firelensDriverName && container.HasSecret(isLogDriverSecretWithGivenProvider) {
			return true
		}
	}
	return false
}

// getAllSSMSecretRequirements stores all secrets in a map whose key is region and value is all
// secrets in that region
func (task *Task) getAllSSMSecretRequirements() map[string][]apicontainer.Secret {
	reqs := make(map[string][]apicontainer.Secret)

	for _, container := range task.Containers {
		for _, secret := range container.Secrets {
			if secret.Provider == apicontainer.SecretProviderSSM {
				if _, ok := reqs[secret.Region]; !ok {
					reqs[secret.Region] = []apicontainer.Secret{}
				}

				reqs[secret.Region] = append(reqs[secret.Region], secret)
			}
		}
	}
	return reqs
}

// requiresASMSecret returns true if at least one container in the task
// needs to retrieve secret from AWS Secrets Manager
func (task *Task) requiresASMSecret() bool {
	for _, container := range task.Containers {
		if container.ShouldCreateWithASMSecret() {
			return true
		}
	}
	return false
}

// initializeASMSecretResource builds the resource dependency map for the asmsecret resource
func (task *Task) initializeASMSecretResource(credentialsManager credentials.Manager,
	resourceFields *taskresource.ResourceFields) {
	asmSecretResource := asmsecret.NewASMSecretResource(task.Arn, task.getAllASMSecretRequirements(),
		task.ExecutionCredentialsID, credentialsManager, resourceFields.ASMClientCreator)
	task.AddResource(asmsecret.ResourceName, asmSecretResource)

	// for every container that needs asm secret vending as envvar, it needs to wait all secrets got retrieved
	for _, container := range task.Containers {
		if container.ShouldCreateWithASMSecret() {
			container.BuildResourceDependency(asmSecretResource.GetName(),
				resourcestatus.ResourceStatus(asmsecret.ASMSecretCreated),
				apicontainerstatus.ContainerCreated)
		}

		// Firelens container needs to depend on secret if other containers use secret log options.
		if container.GetFirelensConfig() != nil && task.firelensDependsOnSecretResource(apicontainer.SecretProviderASM) {
			container.BuildResourceDependency(asmSecretResource.GetName(),
				resourcestatus.ResourceStatus(asmsecret.ASMSecretCreated),
				apicontainerstatus.ContainerCreated)
		}
	}
}

// getAllASMSecretRequirements stores secrets in a task in a map
func (task *Task) getAllASMSecretRequirements() map[string]apicontainer.Secret {
	reqs := make(map[string]apicontainer.Secret)

	for _, container := range task.Containers {
		for _, secret := range container.Secrets {
			if secret.Provider == apicontainer.SecretProviderASM {
				secretKey := secret.GetSecretResourceCacheKey()
				if _, ok := reqs[secretKey]; !ok {
					reqs[secretKey] = secret
				}
			}
		}
	}
	return reqs
}

// GetFirelensContainer returns the firelens container in the task, if there is one.
func (task *Task) GetFirelensContainer() *apicontainer.Container {
	for _, container := range task.Containers {
		if container.GetFirelensConfig() != nil { // This is a firelens container.
			return container
		}
	}
	return nil
}

// initializeFirelensResource initializes the firelens task resource and adds it as a dependency of the
// firelens container.
func (task *Task) initializeFirelensResource(config *config.Config, resourceFields *taskresource.ResourceFields,
	firelensContainer *apicontainer.Container, credentialsManager credentials.Manager) error {
	if firelensContainer.GetFirelensConfig() == nil {
		return errors.New("firelens container config doesn't exist")
	}

	containerToLogOptions := make(map[string]map[string]string)
	// Collect plain text log options.
	if err := task.collectFirelensLogOptions(containerToLogOptions); err != nil {
		return errors.Wrap(err, "unable to initialize firelens resource")
	}

	// Collect secret log options.
	if err := task.collectFirelensLogEnvOptions(containerToLogOptions, firelensContainer.FirelensConfig.Type); err != nil {
		return errors.Wrap(err, "unable to initialize firelens resource")
	}

	for _, container := range task.Containers {
		firelensConfig := container.GetFirelensConfig()
		if firelensConfig != nil { // is Firelens container
			var ec2InstanceID string
			if container.Environment != nil && container.Environment[awsExecutionEnvKey] == ec2ExecutionEnv {
				ec2InstanceID = resourceFields.EC2InstanceID
			}

			var networkMode string
			if task.IsNetworkModeAWSVPC() {
				networkMode = AWSVPCNetworkMode
			} else if container.GetNetworkModeFromHostConfig() == "" || container.GetNetworkModeFromHostConfig() == BridgeNetworkMode {
				networkMode = BridgeNetworkMode
			} else {
				networkMode = container.GetNetworkModeFromHostConfig()
			}
			// Resolve Firelens container memory limit to be used for calculating Firelens memory buffer limit
			// We will use the container 'memoryReservation' (soft limit) if present, otherwise the container 'memory' (hard limit)
			// If neither is present, we will pass in 0 indicating that a memory limit is not specified, and firelens will
			// fall back to use a default memory buffer limit value
			// see - agent/taskresource/firelens/firelensconfig_unix.go:resolveMemBufLimit()
			var containerMemoryLimit int64
			if memoryReservation := container.GetMemoryReservationFromHostConfig(); memoryReservation > 0 {
				containerMemoryLimit = memoryReservation
			} else if container.Memory > 0 {
				containerMemoryLimit = int64(container.Memory)
			} else {
				containerMemoryLimit = 0
			}
			firelensResource, err := firelens.NewFirelensResource(config.Cluster, task.Arn, task.Family+":"+task.Version,
				ec2InstanceID, config.DataDir, firelensConfig.Type, config.AWSRegion, networkMode,
				firelensContainer.GetUser(), firelensConfig.Options, containerToLogOptions, credentialsManager,
				task.ExecutionCredentialsID, containerMemoryLimit, config.InstanceIPCompatibility)

			if err != nil {
				return errors.Wrap(err, "unable to initialize firelens resource")
			}
			task.AddResource(firelens.ResourceName, firelensResource)
			container.BuildResourceDependency(firelensResource.GetName(), resourcestatus.ResourceCreated,
				apicontainerstatus.ContainerCreated)
			return nil
		}
	}

	return errors.New("unable to initialize firelens resource because there's no firelens container")
}

// addFirelensContainerDependency adds a START dependency between each container using awsfirelens log driver
// and the firelens container.
func (task *Task) addFirelensContainerDependency() error {
	var firelensContainer *apicontainer.Container
	for _, container := range task.Containers {
		if container.GetFirelensConfig() != nil {
			firelensContainer = container
		}
	}

	if firelensContainer == nil {
		return errors.New("unable to add firelens container dependency because there's no firelens container")
	}

	if firelensContainer.HasContainerDependencies() {
		// If firelens container has any container dependency, we don't add internal container dependency that depends
		// on it in order to be safe (otherwise we need to deal with circular dependency).
		logger.Warn("Not adding container dependency to let firelens container start first since it has dependency on other containers.", logger.Fields{
			field.TaskID:        task.GetID(),
			"firelensContainer": firelensContainer.Name,
		})
		return nil
	}

	for _, container := range task.Containers {
		containerHostConfig := container.GetHostConfig()
		if containerHostConfig == nil {
			continue
		}

		// Firelens container itself could be using awsfirelens log driver. Don't add container dependency in this case.
		if container.Name == firelensContainer.Name {
			continue
		}

		hostConfig := &dockercontainer.HostConfig{}
		if err := json.Unmarshal([]byte(*containerHostConfig), hostConfig); err != nil {
			return errors.Wrapf(err, "unable to decode host config of container %s", container.Name)
		}

		if hostConfig.LogConfig.Type == firelensDriverName {
			// If there's no dependency between the app container and the firelens container, make firelens container
			// start first to be the default behavior by adding a START container dependency.
			if !container.DependsOnContainer(firelensContainer.Name) {
				logger.Info("Adding a START container dependency on firelens for container", logger.Fields{
					field.TaskID:        task.GetID(),
					"firelensContainer": firelensContainer.Name,
					field.Container:     container.Name,
				})
				container.AddContainerDependency(firelensContainer.Name, ContainerOrderingStartCondition)
			}
		}
	}

	return nil
}

// collectFirelensLogOptions collects the log options for all the containers that use the firelens container
// as the log driver.
// containerToLogOptions is a nested map. Top level key is the container name. Second level is a map storing
// the log option key and value of the container.
func (task *Task) collectFirelensLogOptions(containerToLogOptions map[string]map[string]string) error {
	for _, container := range task.Containers {
		if container.DockerConfig.HostConfig == nil {
			continue
		}

		hostConfig := &dockercontainer.HostConfig{}
		if err := json.Unmarshal([]byte(*container.DockerConfig.HostConfig), hostConfig); err != nil {
			return errors.Wrapf(err, "unable to decode host config of container %s", container.Name)
		}

		if hostConfig.LogConfig.Type == firelensDriverName {
			if containerToLogOptions[container.Name] == nil {
				containerToLogOptions[container.Name] = make(map[string]string)
			}
			for k, v := range hostConfig.LogConfig.Config {
				if k == FirelensLogDriverBufferLimitOption {
					continue
				}
				containerToLogOptions[container.Name][k] = v
			}
		}
	}

	return nil
}

// collectFirelensLogEnvOptions collects all the log secret options. Each secret log option will have a value
// of a config file variable (e.g. "${config_var_name}") and we will pass the secret value as env to the firelens
// container, and it will resolve the config file variable from the env.
// Each config variable name has a format of log-option-key_container-name. We need the container name because options
// from different containers using awsfirelens log driver in a task will be presented in the same firelens config file.
func (task *Task) collectFirelensLogEnvOptions(containerToLogOptions map[string]map[string]string, firelensConfigType string) error {
	placeholderFmt := ""
	switch firelensConfigType {
	case firelens.FirelensConfigTypeFluentd:
		placeholderFmt = firelensConfigVarPlaceholderFmtFluentd
	case firelens.FirelensConfigTypeFluentbit:
		placeholderFmt = firelensConfigVarPlaceholderFmtFluentbit
	default:
		return errors.Errorf("unsupported firelens config type %s", firelensConfigType)
	}

	for _, container := range task.Containers {
		for _, secret := range container.Secrets {
			if secret.Target == apicontainer.SecretTargetLogDriver {
				if containerToLogOptions[container.Name] == nil {
					containerToLogOptions[container.Name] = make(map[string]string)
				}

				idx := task.GetContainerIndex(container.Name)
				if idx < 0 {
					return errors.Errorf("can't find container %s in task %s", container.Name, task.Arn)
				}
				containerToLogOptions[container.Name][secret.Name] = fmt.Sprintf(placeholderFmt,
					fmt.Sprintf(firelensConfigVarFmt, secret.Name, idx))
			}
		}
	}
	return nil
}

// AddFirelensContainerBindMounts adds config file bind mount and socket directory bind mount to the firelens
// container's host config.
func (task *Task) AddFirelensContainerBindMounts(firelensConfig *apicontainer.FirelensConfig, hostConfig *dockercontainer.HostConfig,
	config *config.Config) *apierrors.HostConfigError {
	taskID := task.GetID()

	var configBind, s3ConfigBind, socketBind string
	switch firelensConfig.Type {
	case firelens.FirelensConfigTypeFluentd:
		configBind = fmt.Sprintf(firelensConfigBindFormatFluentd, config.DataDirOnHost, taskID)
		s3ConfigBind = fmt.Sprintf(firelensS3ConfigBindFormat, config.DataDirOnHost, taskID, firelens.S3ConfigPathFluentd)
	case firelens.FirelensConfigTypeFluentbit:
		configBind = fmt.Sprintf(firelensConfigBindFormatFluentbit, config.DataDirOnHost, taskID)
		s3ConfigBind = fmt.Sprintf(firelensS3ConfigBindFormat, config.DataDirOnHost, taskID, firelens.S3ConfigPathFluentbit)
	default:
		return &apierrors.HostConfigError{Msg: fmt.Sprintf("encounter invalid firelens configuration type %s",
			firelensConfig.Type)}
	}
	socketBind = fmt.Sprintf(firelensSocketBindFormat, config.DataDirOnHost, taskID)

	hostConfig.Binds = append(hostConfig.Binds, configBind, socketBind)

	// Add the s3 config bind mount if firelens container is using a config file from S3.
	if firelensConfig.Options != nil && firelensConfig.Options[firelens.ExternalConfigTypeOption] == firelens.ExternalConfigTypeS3 {
		hostConfig.Binds = append(hostConfig.Binds, s3ConfigBind)
	}
	return nil
}

// IsNetworkModeAWSVPC checks if the task is configured to use the AWSVPC task networking feature.
func (task *Task) IsNetworkModeAWSVPC() bool {
	return task.NetworkMode == AWSVPCNetworkMode
}

// IsNetworkModeBridge checks if the task is configured to use the bridge network mode.
func (task *Task) IsNetworkModeBridge() bool {
	return task.NetworkMode == BridgeNetworkMode
}

// IsNetworkModeHost checks if the task is configured to use the host network mode.
func (task *Task) IsNetworkModeHost() bool {
	return task.NetworkMode == HostNetworkMode
}

func (task *Task) addNetworkResourceProvisioningDependency(cfg *config.Config) error {
	if task.IsNetworkModeAWSVPC() {
		return task.addNetworkResourceProvisioningDependencyAwsvpc(cfg)
	} else if task.IsNetworkModeBridge() && task.IsServiceConnectEnabled() {
		return task.addNetworkResourceProvisioningDependencyServiceConnectBridge(cfg)
	}
	return nil
}

func (task *Task) addNetworkResourceProvisioningDependencyAwsvpc(cfg *config.Config) error {
	pauseContainer := apicontainer.NewContainerWithSteadyState(apicontainerstatus.ContainerResourcesProvisioned)
	pauseContainer.TransitionDependenciesMap = make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet)
	pauseContainer.Name = NetworkPauseContainerName
	pauseContainer.Image = fmt.Sprintf("%s:%s", cfg.PauseContainerImageName, cfg.PauseContainerTag)
	pauseContainer.Essential = true
	pauseContainer.Type = apicontainer.ContainerCNIPause

	// Set pauseContainer user the same as proxy container user when image name is not DefaultPauseContainerImageName
	if task.GetAppMesh() != nil && cfg.PauseContainerImageName != config.DefaultPauseContainerImageName {
		appMeshConfig := task.GetAppMesh()

		// Validation is done when registering task to make sure there is one container name matching
		for _, container := range task.Containers {
			if container.Name != appMeshConfig.ContainerName {
				continue
			}

			if container.DockerConfig.Config == nil {
				return errors.Errorf("user needs to be specified for proxy container")
			}
			containerConfig := &dockercontainer.Config{}
			if err := json.Unmarshal([]byte(aws.ToString(container.DockerConfig.Config)), &containerConfig); err != nil {
				return errors.Errorf("unable to decode given docker config: %s", err.Error())
			}

			if containerConfig.User == "" {
				return errors.Errorf("user needs to be specified for proxy container")
			}

			pauseConfig := dockercontainer.Config{
				User:  containerConfig.User,
				Image: fmt.Sprintf("%s:%s", cfg.PauseContainerImageName, cfg.PauseContainerTag),
			}

			bytes, err := json.Marshal(pauseConfig)
			if err != nil {
				return errors.Errorf("Error json marshaling pause config: %s", err)
			}
			serializedConfig := string(bytes)
			pauseContainer.DockerConfig = apicontainer.DockerConfig{
				Config: &serializedConfig,
			}
			break
		}
	}

	task.Containers = append(task.Containers, pauseContainer)

	for _, container := range task.Containers {
		if container.IsInternal() {
			continue
		}
		container.BuildContainerDependency(NetworkPauseContainerName, apicontainerstatus.ContainerResourcesProvisioned, apicontainerstatus.ContainerManifestPulled)
		pauseContainer.BuildContainerDependency(container.Name, apicontainerstatus.ContainerStopped, apicontainerstatus.ContainerStopped)
	}

	for _, resource := range task.GetResources() {
		if resource.DependOnTaskNetwork() {
			logger.Debug("Adding network pause container dependency to resource", logger.Fields{
				field.TaskID:   task.GetID(),
				field.Resource: resource.GetName(),
			})
			resource.BuildContainerDependency(NetworkPauseContainerName, apicontainerstatus.ContainerResourcesProvisioned, resourcestatus.ResourceStatus(taskresourcevolume.VolumeCreated))
		}
	}
	return nil
}

// addNetworkResourceProvisioningDependencyServiceConnectBridge creates one pause container per task container
// including SC container, and add a dependency for SC container to wait for all pause container RESOURCES_PROVISIONED.
//
// SC pause container will use CNI plugin for configuring tproxy, while other pause container(s) will configure ip route
// to send SC traffic to SC container
func (task *Task) addNetworkResourceProvisioningDependencyServiceConnectBridge(cfg *config.Config) error {
	scContainer := task.GetServiceConnectContainer()
	var scPauseContainer *apicontainer.Container
	for _, container := range task.Containers {
		if container.IsInternal() {
			continue
		}
		pauseContainer := apicontainer.NewContainerWithSteadyState(apicontainerstatus.ContainerResourcesProvisioned)
		pauseContainer.TransitionDependenciesMap = make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet)
		// The pause container name is used internally by task engine but still needs to be unique for every task,
		// hence we are appending the corresponding application container name (which must already be unique within the task)
		pauseContainer.Name = fmt.Sprintf(ServiceConnectPauseContainerNameFormat, container.Name)
		pauseContainer.Image = fmt.Sprintf("%s:%s", cfg.PauseContainerImageName, cfg.PauseContainerTag)
		pauseContainer.Essential = true
		pauseContainer.Type = apicontainer.ContainerCNIPause

		task.Containers = append(task.Containers, pauseContainer)
		// SC container CREATED will depend on ALL pause containers RESOURCES_PROVISIONED
		scContainer.BuildContainerDependency(pauseContainer.Name, apicontainerstatus.ContainerResourcesProvisioned, apicontainerstatus.ContainerCreated)
		pauseContainer.BuildContainerDependency(scContainer.Name, apicontainerstatus.ContainerStopped, apicontainerstatus.ContainerStopped)
		if container == scContainer {
			scPauseContainer = pauseContainer
		}
	}

	// All other task pause container RESOURCES_PROVISIONED depends on SC pause container RUNNING because task pause container
	// CNI plugin invocation needs the IP of SC pause container (to send SC traffic to)
	for _, container := range task.Containers {
		if container.Type != apicontainer.ContainerCNIPause || container == scPauseContainer {
			continue
		}
		container.BuildContainerDependency(scPauseContainer.Name, apicontainerstatus.ContainerRunning, apicontainerstatus.ContainerResourcesProvisioned)
	}
	return nil
}

// GetBridgeModePauseContainerForTaskContainer retrieves the associated pause container for a task container (SC container
// or customer-defined containers) in a bridge-mode SC-enabled task.
// For a container with name "abc", the pause container will always be named "~internal~ecs~pause-abc"
func (task *Task) GetBridgeModePauseContainerForTaskContainer(container *apicontainer.Container) (*apicontainer.Container, error) {
	// "~internal~ecs~pause-$TASK_CONTAINER_NAME"
	pauseContainerName := fmt.Sprintf(ServiceConnectPauseContainerNameFormat, container.Name)
	pauseContainer, ok := task.ContainerByName(pauseContainerName)
	if !ok {
		return nil, fmt.Errorf("could not find pause container %s for task container %s", pauseContainerName, container.Name)
	}
	return pauseContainer, nil
}

// getBridgeModeTaskContainerForPauseContainer retrieves the associated task container for a pause container in a bridge-mode SC-enabled task.
// For a container with name "abc", the pause container will always be named "~internal~ecs~pause-abc"
func (task *Task) getBridgeModeTaskContainerForPauseContainer(container *apicontainer.Container) (*apicontainer.Container, error) {
	if container.Type != apicontainer.ContainerCNIPause {
		return nil, fmt.Errorf("container %s is not a CNI pause container", container.Name)
	}
	// limit the result to 2 substrings as $TASK_CONTAINER_NAME may also container '-'
	stringSlice := strings.SplitN(container.Name, "-", 2)
	if len(stringSlice) < 2 {
		return nil, fmt.Errorf("SC bridge mode pause container %s does not conform to %s-$TASK_CONTAINER_NAME format", container.Name, NetworkPauseContainerName)
	}
	taskContainerName := stringSlice[1]
	taskContainer, ok := task.ContainerByName(taskContainerName)
	if !ok {
		return nil, fmt.Errorf("could not find task container %s for pause container %s", taskContainerName, container.Name)
	}
	return taskContainer, nil
}

func (task *Task) addNamespaceSharingProvisioningDependency(cfg *config.Config) {
	// Pause container does not need to be created if no namespace sharing will be done at task level
	if task.getIPCMode() != ipcModeTask && task.getPIDMode() != pidModeTask {
		return
	}
	namespacePauseContainer := apicontainer.NewContainerWithSteadyState(apicontainerstatus.ContainerRunning)
	namespacePauseContainer.TransitionDependenciesMap = make(map[apicontainerstatus.ContainerStatus]apicontainer.TransitionDependencySet)
	namespacePauseContainer.Name = NamespacePauseContainerName
	namespacePauseContainer.Image = fmt.Sprintf("%s:%s", config.DefaultPauseContainerImageName, config.DefaultPauseContainerTag)
	namespacePauseContainer.Essential = true
	namespacePauseContainer.Type = apicontainer.ContainerNamespacePause
	task.Containers = append(task.Containers, namespacePauseContainer)

	for _, container := range task.Containers {
		if container.IsInternal() {
			continue
		}
		container.BuildContainerDependency(NamespacePauseContainerName, apicontainerstatus.ContainerRunning, apicontainerstatus.ContainerPulled)
		namespacePauseContainer.BuildContainerDependency(container.Name, apicontainerstatus.ContainerStopped, apicontainerstatus.ContainerStopped)
	}
}

// ContainerByName returns the *Container for the given name
func (task *Task) ContainerByName(name string) (*apicontainer.Container, bool) {
	for _, container := range task.Containers {
		if container.Name == name {
			return container, true
		}
	}
	return nil, false
}

// HostVolumeByName returns the task Volume for the given a volume name in that
// task. The second return value indicates the presence of that volume
func (task *Task) HostVolumeByName(name string) (taskresourcevolume.Volume, bool) {
	for _, v := range task.Volumes {
		if v.Name == name {
			return v.Volume, true
		}
	}
	return nil, false
}

// UpdateMountPoints updates the mount points of volumes that were created
// without specifying a host path.  This is used as part of the empty host
// volume feature.
func (task *Task) UpdateMountPoints(cont *apicontainer.Container, vols []types.MountPoint) {
	for _, mountPoint := range cont.MountPoints {
		containerPath := utils.GetCanonicalPath(mountPoint.ContainerPath)
		for _, vol := range vols {
			if strings.Compare(vol.Destination, containerPath) == 0 ||
				// /path/ -> /path or \path\ -> \path
				strings.Compare(vol.Destination, strings.TrimRight(containerPath, string(filepath.Separator))) == 0 {
				if hostVolume, exists := task.HostVolumeByName(mountPoint.SourceVolume); exists {
					if empty, ok := hostVolume.(*taskresourcevolume.LocalDockerVolume); ok {
						empty.HostPath = vol.Source
					}
				}
			}
		}
	}
}

// updateTaskKnownStatus updates the given task's status based on its container's status.
// It updates to the minimum of all containers no matter what
// It returns a TaskStatus indicating what change occurred or TaskStatusNone if
// there was no change
// Invariant: task known status is the minimum of container known status
func (task *Task) updateTaskKnownStatus() (newStatus apitaskstatus.TaskStatus) {
	logger.Debug("Updating task's known status", logger.Fields{
		field.TaskID: task.GetID(),
	})
	// Set to a large 'impossible' status that can't be the min
	containerEarliestKnownStatus := apicontainerstatus.ContainerZombie
	var earliestKnownStatusContainer, essentialContainerStopped *apicontainer.Container
	for _, container := range task.Containers {
		containerKnownStatus := container.GetKnownStatus()
		if containerKnownStatus == apicontainerstatus.ContainerStopped && container.Essential {
			essentialContainerStopped = container
		}
		if containerKnownStatus < containerEarliestKnownStatus {
			containerEarliestKnownStatus = containerKnownStatus
			earliestKnownStatusContainer = container
		}
	}
	if earliestKnownStatusContainer == nil {
		logger.Critical("Impossible state found while updating task's known status", logger.Fields{
			field.TaskID:          task.GetID(),
			"earliestKnownStatus": containerEarliestKnownStatus.String(),
		})
		return apitaskstatus.TaskStatusNone
	}
	logger.Debug("Found container with earliest known status", logger.Fields{
		field.TaskID:        task.GetID(),
		field.Container:     earliestKnownStatusContainer.Name,
		field.KnownStatus:   earliestKnownStatusContainer.GetKnownStatus().String(),
		field.DesiredStatus: earliestKnownStatusContainer.GetDesiredStatus().String(),
	})
	// If the essential container is stopped while other containers may be running
	// don't update the task status until the other containers are stopped.
	if earliestKnownStatusContainer.IsKnownSteadyState() && essentialContainerStopped != nil {
		logger.Debug("Essential container is stopped while other containers are running, not updating task status", logger.Fields{
			field.TaskID:    task.GetID(),
			field.Container: essentialContainerStopped.Name,
		})
		return apitaskstatus.TaskStatusNone
	}
	// We can't rely on earliest container known status alone for determining if the
	// task state needs to be updated as containers can have different steady states
	// defined. Instead, we should get the task status for all containers' known
	// statuses and compute the min of this
	earliestKnownTaskStatus := task.getEarliestKnownTaskStatusForContainers()
	if task.GetKnownStatus() < earliestKnownTaskStatus {
		task.SetKnownStatus(earliestKnownTaskStatus)
		logger.Info("Container change also resulted in task change", logger.Fields{
			field.TaskID:        task.GetID(),
			field.Container:     earliestKnownStatusContainer.Name,
			field.RuntimeID:     earliestKnownStatusContainer.RuntimeID,
			field.DesiredStatus: task.GetDesiredStatus().String(),
			field.KnownStatus:   earliestKnownTaskStatus.String(),
		})
		return earliestKnownTaskStatus
	}
	return apitaskstatus.TaskStatusNone
}

// getEarliestKnownTaskStatusForContainers gets the lowest (earliest) task status
// based on the known statuses of all containers in the task
func (task *Task) getEarliestKnownTaskStatusForContainers() apitaskstatus.TaskStatus {
	if len(task.Containers) == 0 {
		logger.Critical("No containers in the task", logger.Fields{
			field.TaskID: task.GetID(),
		})
		return apitaskstatus.TaskStatusNone
	}
	// Set earliest container status to an impossible to reach 'high' task status
	earliest := apitaskstatus.TaskZombie
	for _, container := range task.Containers {
		containerTaskStatus := apitaskstatus.MapContainerToTaskStatus(container.GetKnownStatus(), container.GetSteadyStateStatus())
		if containerTaskStatus < earliest {
			earliest = containerTaskStatus
		}
	}

	return earliest
}

// DockerConfig converts the given container in this task to the format of
// the Docker SDK 'Config' struct
func (task *Task) DockerConfig(container *apicontainer.Container, apiVersion dockerclient.DockerVersion) (*dockercontainer.Config, *apierrors.DockerClientConfigError) {
	return task.dockerConfig(container, apiVersion)
}

func (task *Task) dockerConfig(container *apicontainer.Container, apiVersion dockerclient.DockerVersion) (*dockercontainer.Config, *apierrors.DockerClientConfigError) {
	dockerEnv := make([]string, 0, len(container.Environment))
	for envKey, envVal := range container.Environment {
		dockerEnv = append(dockerEnv, envKey+"="+envVal)
	}

	var entryPoint []string
	if container.EntryPoint != nil {
		entryPoint = *container.EntryPoint
	}

	var exposedPorts nat.PortSet
	var err error
	if exposedPorts, err = task.dockerExposedPorts(container); err != nil {
		return nil, &apierrors.DockerClientConfigError{Msg: "error resolving docker exposed ports for container: " + err.Error()}
	}

	containerConfig := &dockercontainer.Config{
		Image:        container.Image,
		Cmd:          container.Command,
		Entrypoint:   entryPoint,
		ExposedPorts: exposedPorts,
		Env:          dockerEnv,
	}

	// TODO [SC] - Move this as well as 'dockerExposedPorts' SC-specific logic into a separate file
	if (task.IsServiceConnectEnabled() && container == task.GetServiceConnectContainer()) ||
		container.Type == apicontainer.ContainerServiceConnectRelay {
		containerConfig.User = strconv.Itoa(serviceconnect.AppNetUID)
	}

	if container.DockerConfig.Config != nil {
		if err := json.Unmarshal([]byte(aws.ToString(container.DockerConfig.Config)), &containerConfig); err != nil {
			return nil, &apierrors.DockerClientConfigError{Msg: "Unable decode given docker config: " + err.Error()}
		}
	}

	if container.HealthCheckType == apicontainer.DockerHealthCheckType && containerConfig.Healthcheck == nil {
		return nil, &apierrors.DockerClientConfigError{
			Msg: "docker health check is nil while container health check type is DOCKER"}
	}

	if containerConfig.Labels == nil {
		containerConfig.Labels = make(map[string]string)
	}

	if container.Type == apicontainer.ContainerCNIPause && task.IsNetworkModeAWSVPC() {
		// apply hostname to pause container's docker config
		return task.applyENIHostname(containerConfig), nil
	}

	return containerConfig, nil
}

// dockerExposedPorts returns the container ports that need to be exposed for a container
//  1. For bridge-mode ServiceConnect-enabled tasks:
//     1a. Pause containers need to expose the port(s) for their associated task container. In particular, SC pause container
//
// needs to expose all listener ports for SC container
//
//	1b. Other containers (customer-defined task containers as well as SC container) will not expose any ports as they are
//
// already exposed through pause container
// 2. For all other tasks, we expose the application container ports.
func (task *Task) dockerExposedPorts(container *apicontainer.Container) (dockerExposedPorts nat.PortSet, err error) {
	containerToCheck := container
	scContainer := task.GetServiceConnectContainer()
	dockerExposedPorts = make(map[nat.Port]struct{})

	if task.IsServiceConnectEnabled() && task.IsNetworkModeBridge() {
		if container.Type == apicontainer.ContainerCNIPause {
			// find the task container associated with this particular pause container, and let pause container
			// expose the application container port
			containerToCheck, err = task.getBridgeModeTaskContainerForPauseContainer(container)
			if err != nil {
				return nil, err
			}
			// if the associated task container is SC container, expose all its ingress and egress listener ports if present
			if containerToCheck == scContainer {
				for _, ic := range task.ServiceConnectConfig.IngressConfig {
					dockerPort := nat.Port(strconv.Itoa(int(ic.ListenerPort))) + "/tcp"
					dockerExposedPorts[dockerPort] = struct{}{}
				}
				ec := task.ServiceConnectConfig.EgressConfig
				if ec != nil { // it's possible that task does not have an egress listener
					dockerPort := nat.Port(strconv.Itoa(int(ec.ListenerPort))) + "/tcp"
					dockerExposedPorts[dockerPort] = struct{}{}
				}
				return dockerExposedPorts, nil
			}
		} else {
			// This is a task container which is launched with "--network container:$pause_container_id"
			// In such case we don't expose any ports (docker won't allow anyway) because they are exposed by their
			// pause container instead.
			return dockerExposedPorts, nil
		}
	}

	for _, portBinding := range containerToCheck.Ports {
		protocol := portBinding.Protocol.String()
		// per port binding config, either one of ContainerPort or ContainerPortRange is set
		if portBinding.ContainerPort != 0 {
			dockerPort := nat.Port(strconv.Itoa(int(portBinding.ContainerPort)) + "/" + protocol)
			dockerExposedPorts[dockerPort] = struct{}{}
		} else if portBinding.ContainerPortRange != "" {
			// we supply containerPortRange here to ensure every port exposed through ECS task definitions
			// will be reported as Config.ExposedPorts in Docker
			startContainerPort, endContainerPort, err := nat.ParsePortRangeToInt(portBinding.ContainerPortRange)
			if err != nil {
				return nil, err
			}

			for i := startContainerPort; i <= endContainerPort; i++ {
				dockerPort := nat.Port(strconv.Itoa(i) + "/" + protocol)
				dockerExposedPorts[dockerPort] = struct{}{}
			}
		}
	}
	return dockerExposedPorts, nil
}

// DockerHostConfig construct the configuration recognized by docker
func (task *Task) DockerHostConfig(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer, apiVersion dockerclient.DockerVersion, cfg *config.Config) (*dockercontainer.HostConfig, *apierrors.HostConfigError) {
	return task.dockerHostConfig(container, dockerContainerMap, apiVersion, cfg)
}

// ApplyExecutionRoleLogsAuth will check whether the task has execution role
// credentials, and add the generated credentials endpoint to the associated HostConfig
func (task *Task) ApplyExecutionRoleLogsAuth(hostConfig *dockercontainer.HostConfig, credentialsManager credentials.Manager) *apierrors.HostConfigError {
	id := task.GetExecutionCredentialsID()
	if id == "" {
		// No execution credentials set for the task. Do not inject the endpoint environment variable.
		return &apierrors.HostConfigError{Msg: "No execution credentials set for the task"}
	}

	executionRoleCredentials, ok := credentialsManager.GetTaskCredentials(id)
	if !ok {
		// Task has credentials id set, but credentials manager is unaware of
		// the id. This should never happen as the payload handler sets
		// credentialsId for the task after adding credentials to the
		// credentials manager
		return &apierrors.HostConfigError{Msg: "Unable to get execution role credentials for task"}
	}
	credentialsEndpointRelativeURI := executionRoleCredentials.IAMRoleCredentials.GenerateCredentialsEndpointRelativeURI()
	if hostConfig.LogConfig.Config == nil {
		hostConfig.LogConfig.Config = map[string]string{}
	}
	logger.Info("Applying execution role credentials to container log auth", logger.Fields{
		field.TaskARN:           executionRoleCredentials.ARN,
		field.RoleType:          executionRoleCredentials.IAMRoleCredentials.RoleType,
		field.RoleARN:           executionRoleCredentials.IAMRoleCredentials.RoleArn,
		field.CredentialsID:     executionRoleCredentials.IAMRoleCredentials.CredentialsID,
		awslogsCredsEndpointOpt: credentialsEndpointRelativeURI,
	})
	hostConfig.LogConfig.Config[awslogsCredsEndpointOpt] = credentialsEndpointRelativeURI
	return nil
}

func (task *Task) dockerHostConfig(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer, apiVersion dockerclient.DockerVersion, cfg *config.Config) (*dockercontainer.HostConfig, *apierrors.HostConfigError) {
	dockerLinkArr, err := task.dockerLinks(container, dockerContainerMap)
	if err != nil {
		return nil, &apierrors.HostConfigError{Msg: err.Error()}
	}
	dockerPortMap, err := task.dockerPortMap(container, cfg.DynamicHostPortRange)
	if err != nil {
		return nil, &apierrors.HostConfigError{Msg: fmt.Sprintf("error retrieving docker port map: %+v", err.Error())}
	}

	volumesFrom, err := task.dockerVolumesFrom(container, dockerContainerMap)
	if err != nil {
		return nil, &apierrors.HostConfigError{Msg: err.Error()}
	}

	binds, err := task.dockerHostBinds(container)
	if err != nil {
		return nil, &apierrors.HostConfigError{Msg: err.Error()}
	}

	resources := task.getDockerResources(container, cfg)

	// Populate hostConfig
	hostConfig := &dockercontainer.HostConfig{
		Links:        dockerLinkArr,
		Binds:        binds,
		PortBindings: dockerPortMap,
		VolumesFrom:  volumesFrom,
		Resources:    resources,
	}

	if err := task.overrideContainerRuntime(container, hostConfig, cfg); err != nil {
		return nil, err
	}

	if container.DockerConfig.HostConfig != nil {
		err := json.Unmarshal([]byte(*container.DockerConfig.HostConfig), hostConfig)
		if err != nil {
			return nil, &apierrors.HostConfigError{Msg: "Unable to decode given host config: " + err.Error()}
		}
	}

	if err := task.platformHostConfigOverride(hostConfig); err != nil {
		return nil, &apierrors.HostConfigError{Msg: err.Error()}
	}

	// Determine if network mode should be overridden and override it if needed
	ok, networkMode := task.shouldOverrideNetworkMode(container, dockerContainerMap)
	if ok {
		hostConfig.NetworkMode = dockercontainer.NetworkMode(networkMode)
		// Override 'awsvpc' parameters if needed
		if container.Type == apicontainer.ContainerCNIPause && task.IsNetworkModeAWSVPC() {
			// apply ExtraHosts to HostConfig for pause container
			if hosts := task.generateENIExtraHosts(); hosts != nil {
				hostConfig.ExtraHosts = append(hostConfig.ExtraHosts, hosts...)
			}

			if task.shouldEnableIPv6() {
				// By default, the disable ipv6 setting is turned on, so need to turn it off to enable it.
				enableIPv6SysctlSetting(hostConfig)
			}

			// Override the DNS settings for the pause container if ENI has custom
			// DNS settings
			return task.overrideDNS(hostConfig), nil
		}
	}

	task.pidModeOverride(container, dockerContainerMap, hostConfig)
	task.ipcModeOverride(container, dockerContainerMap, hostConfig)

	return hostConfig, nil
}

// overrideContainerRuntime overrides the runtime for the container in host config if needed.
func (task *Task) overrideContainerRuntime(container *apicontainer.Container, hostCfg *dockercontainer.HostConfig,
	cfg *config.Config) *apierrors.HostConfigError {
	if task.isGPUEnabled() && task.shouldRequireNvidiaRuntime(container) {
		if !cfg.External.Enabled() {
			if task.NvidiaRuntime == "" {
				return &apierrors.HostConfigError{Msg: "Runtime is not set for GPU containers"}
			}
			logger.Debug("Setting runtime for container", logger.Fields{
				field.TaskID:    task.GetID(),
				field.Container: container.Name,
				"runTime":       task.NvidiaRuntime,
			})
			hostCfg.Runtime = task.NvidiaRuntime
		}
	}

	if cfg.InferentiaSupportEnabled && container.RequireNeuronRuntime() {
		logger.Debug("Setting runtime for container", logger.Fields{
			field.TaskID:    task.GetID(),
			field.Container: container.Name,
			"runTime":       neuronRuntime,
		})
		hostCfg.Runtime = neuronRuntime
	}
	return nil
}

// Requires an *apicontainer.Container and returns the Resources for the HostConfig struct
func (task *Task) getDockerResources(container *apicontainer.Container, cfg *config.Config) dockercontainer.Resources {
	// Convert MB to B and set Memory
	dockerMem := int64(container.Memory * 1024 * 1024)
	if dockerMem != 0 && dockerMem < apicontainer.DockerContainerMinimumMemoryInBytes {
		logger.Warn("Memory setting too low for container, increasing to minimum", logger.Fields{
			field.TaskID:    task.GetID(),
			field.Container: container.Name,
			"bytes":         apicontainer.DockerContainerMinimumMemoryInBytes,
		})
		dockerMem = apicontainer.DockerContainerMinimumMemoryInBytes
	}
	// Set CPUShares
	cpuShare := task.dockerCPUShares(container.CPU)
	resources := dockercontainer.Resources{
		Memory:    dockerMem,
		CPUShares: cpuShare,
	}
	if cfg.External.Enabled() && cfg.GPUSupportEnabled {
		deviceRequest := dockercontainer.DeviceRequest{
			Capabilities: [][]string{[]string{"gpu"}},
			DeviceIDs:    container.GPUIDs,
		}
		resources.DeviceRequests = []dockercontainer.DeviceRequest{deviceRequest}
	}
	return resources
}

// shouldOverrideNetworkMode returns true if the network mode of the container needs
// to be overridden. It also returns the override string in this case. It returns
// false otherwise
func (task *Task) shouldOverrideNetworkMode(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer) (bool, string) {
	// TODO. We can do an early return here by determining which kind of task it is
	// Example: Does this task have ENIs in its payload, what is its networking mode etc
	if container.IsInternal() {
		// If it's a CNI pause container, set the network mode to none for awsvpc, set to bridge if task is using
		// bridge mode and this is an SC-enabled task.
		// If it's a ServiceConnect relay container, the container is internally managed, and should keep its "host"
		// network mode by design
		// Other internal containers are either for creating empty host volumes or for creating the 'pause' container.
		// Both of these only need the network mode to be set to "none"
		if container.Type == apicontainer.ContainerCNIPause {
			if task.IsNetworkModeAWSVPC() {
				return true, networkModeNone
			} else if task.IsNetworkModeBridge() && task.IsServiceConnectEnabled() {
				return true, BridgeNetworkMode
			}
		}
		if container.Type == apicontainer.ContainerServiceConnectRelay {
			return false, ""
		}
		return true, networkModeNone
	}

	// For other types of containers, determine if the container map contains
	// a pause container. Since a pause container is only added to the task
	// when using non docker daemon supported network modes, its existence
	// indicates the need to configure the network mode outside of supported
	// network drivers
	if task.IsNetworkModeAWSVPC() {
		return task.shouldOverrideNetworkModeAwsvpc(container, dockerContainerMap)
	} else if task.IsNetworkModeBridge() && task.IsServiceConnectEnabled() {
		return task.shouldOverrideNetworkModeServiceConnectBridge(container, dockerContainerMap)
	}
	return false, ""
}

func (task *Task) shouldOverrideNetworkModeAwsvpc(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer) (bool, string) {
	pauseContName := ""
	for _, cont := range task.Containers {
		if cont.Type == apicontainer.ContainerCNIPause {
			pauseContName = cont.Name
			break
		}
	}
	if pauseContName == "" {
		logger.Critical("Pause container required, but not found in the task", logger.Fields{
			field.TaskID: task.GetID(),
		})
		return false, ""
	}
	pauseContainer, ok := dockerContainerMap[pauseContName]
	if !ok || pauseContainer == nil {
		// This should never be the case and implies a code-bug.
		logger.Critical("Pause container required, but not found in container map", logger.Fields{
			field.TaskID:    task.GetID(),
			field.Container: container.Name,
		})
		return false, ""
	}
	return true, dockerMappingContainerPrefix + pauseContainer.DockerID
}

// shouldOverrideNetworkModeServiceConnectBridge checks if a bridge-mode SC task container needs network mode override
// For non-internal containers in an SC bridge-mode task, each gets a pause container, and should be launched
// with container network mode use pause container netns (the "docker run" equivalent option is
// "--network container:$pause_container_id")
func (task *Task) shouldOverrideNetworkModeServiceConnectBridge(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer) (bool, string) {
	pauseContainer, err := task.GetBridgeModePauseContainerForTaskContainer(container)
	if err != nil {
		// This should never be the case and implies a code-bug.
		logger.Critical("Pause container required per task container for Service Connect task bridge mode, but "+
			"not found for task container", logger.Fields{
			field.TaskID:    task.GetID(),
			field.Container: container.Name,
		})
		return false, ""
	}
	dockerPauseContainer, ok := dockerContainerMap[pauseContainer.Name]
	if !ok || dockerPauseContainer == nil {
		// This should never be the case and implies a code-bug.
		logger.Critical("Pause container required per task container for Service Connect task bridge mode, but "+
			"not found in docker container map for task container", logger.Fields{
			field.TaskID:    task.GetID(),
			field.Container: container.Name,
		})
		return false, ""
	}
	return true, dockerMappingContainerPrefix + dockerPauseContainer.DockerID
}

// overrideDNS overrides a container's host config if the following conditions are
// true:
// 1. Task has an ENI associated with it
// 2. ENI has custom DNS IPs and search list associated with it
// This should only be done for the pause container as other containers inherit
// /etc/resolv.conf of this container (they share the network namespace)
func (task *Task) overrideDNS(hostConfig *dockercontainer.HostConfig) *dockercontainer.HostConfig {
	eni := task.GetPrimaryENI()
	if eni == nil {
		return hostConfig
	}

	hostConfig.DNS = eni.DomainNameServers
	hostConfig.DNSSearch = eni.DomainNameSearchList

	return hostConfig
}

// applyENIHostname adds the hostname provided by the ENI message to the
// container's docker config. At the time of implmentation, we are only using it
// to configure the pause container for awsvpc tasks
func (task *Task) applyENIHostname(dockerConfig *dockercontainer.Config) *dockercontainer.Config {
	eni := task.GetPrimaryENI()
	if eni == nil {
		return dockerConfig
	}

	hostname := eni.GetHostname()
	if hostname == "" {
		return dockerConfig
	}

	dockerConfig.Hostname = hostname
	return dockerConfig
}

// generateENIExtraHosts returns a slice of strings of the form "hostname:ip"
// that is generated using the hostname and ip addresses allocated to the ENI
func (task *Task) generateENIExtraHosts() []string {
	eni := task.GetPrimaryENI()
	if eni == nil {
		return nil
	}

	hostname := eni.GetHostname()
	if hostname == "" {
		return nil
	}

	extraHosts := []string{}

	ipAddresses := eni.GetIPV4Addresses()
	if eni.IPv6Only() {
		// Use IPv6 addresses only if task ENI is IPv6-only.
		// Historically, IPv6 addresses are not added for dual stack ENIs.
		ipAddresses = eni.GetIPV6Addresses()
	}

	for _, ip := range ipAddresses {
		host := fmt.Sprintf("%s:%s", hostname, ip)
		extraHosts = append(extraHosts, host)
	}

	return extraHosts
}

func (task *Task) shouldEnableIPv4() bool {
	eni := task.GetPrimaryENI()
	if eni == nil {
		return false
	}
	return len(eni.GetIPV4Addresses()) > 0
}

func (task *Task) shouldEnableIPv6() bool {
	eni := task.GetPrimaryENI()
	if eni == nil {
		return false
	}
	return len(eni.GetIPV6Addresses()) > 0
}

func setPIDMode(hostConfig *dockercontainer.HostConfig, pidMode string) {
	hostConfig.PidMode = dockercontainer.PidMode(pidMode)
}

// pidModeOverride sets the PIDMode of the container if needed
func (task *Task) pidModeOverride(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer, hostConfig *dockercontainer.HostConfig) {
	// If the container is an internal container (ContainerEmptyHostVolume,
	// ContainerCNIPause, or ContainerNamespacePause), then PID namespace for
	// the container itself should be private (default Docker option)
	if container.IsInternal() {
		return
	}

	switch task.getPIDMode() {
	case pidModeHost:
		setPIDMode(hostConfig, pidModeHost)
		return

	case pidModeTask:
		pauseCont, ok := task.ContainerByName(NamespacePauseContainerName)
		if !ok {
			logger.Critical("Namespace Pause container not found; stopping task", logger.Fields{
				field.TaskID: task.GetID(),
			})
			task.SetDesiredStatus(apitaskstatus.TaskStopped)
			return
		}
		pauseDockerID, ok := dockerContainerMap[pauseCont.Name]
		if !ok || pauseDockerID == nil {
			// Docker container shouldn't be nil or not exist if the Container definition within task exists; implies code-bug
			logger.Critical("Namespace Pause docker container not found; stopping task", logger.Fields{
				field.TaskID: task.GetID(),
			})
			task.SetDesiredStatus(apitaskstatus.TaskStopped)
			return
		}
		setPIDMode(hostConfig, dockerMappingContainerPrefix+pauseDockerID.DockerID)
		return

		// If PIDMode is not Host or Task, then no need to override
	default:
		break
	}
}

func setIPCMode(hostConfig *dockercontainer.HostConfig, mode string) {
	hostConfig.IpcMode = dockercontainer.IpcMode(mode)
}

// ipcModeOverride will override the IPCMode of the container if needed
func (task *Task) ipcModeOverride(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer, hostConfig *dockercontainer.HostConfig) {
	// All internal containers do not need the same IPCMode. The NamespaceContainerPause
	// needs to be "shareable" if ipcMode is "task". All other internal containers should
	// defer to the Docker daemon default option (either shareable or private depending on
	// version and configuration)
	if container.IsInternal() {
		if container.Type == apicontainer.ContainerNamespacePause {
			// Setting NamespaceContainerPause to be sharable with other containers
			if task.getIPCMode() == ipcModeTask {
				setIPCMode(hostConfig, ipcModeSharable)
				return
			}
		}
		// Defaulting to Docker daemon default option
		return
	}

	switch task.getIPCMode() {
	// IPCMode is none - container will have own private namespace with /dev/shm not mounted
	case ipcModeNone:
		setIPCMode(hostConfig, ipcModeNone)
		return

	case ipcModeHost:
		setIPCMode(hostConfig, ipcModeHost)
		return

	case ipcModeTask:
		pauseCont, ok := task.ContainerByName(NamespacePauseContainerName)
		if !ok {
			logger.Critical("Namespace Pause container not found; stopping task", logger.Fields{
				field.TaskID: task.GetID(),
			})
			task.SetDesiredStatus(apitaskstatus.TaskStopped)
			break
		}
		pauseDockerID, ok := dockerContainerMap[pauseCont.Name]
		if !ok || pauseDockerID == nil {
			// Docker container shouldn't be nil or not exist if the Container definition within task exists; implies code-bug
			logger.Critical("Namespace Pause container not found; stopping task", logger.Fields{
				field.TaskID: task.GetID(),
			})
			task.SetDesiredStatus(apitaskstatus.TaskStopped)
			break
		}
		setIPCMode(hostConfig, dockerMappingContainerPrefix+pauseDockerID.DockerID)
		return

	default:
		break
	}
}

func (task *Task) initializeContainerOrdering() error {
	// Handle ordering for Service Connect
	if task.IsServiceConnectEnabled() {
		scContainer := task.GetServiceConnectContainer()

		for _, container := range task.Containers {
			if container.IsInternal() || container == scContainer {
				continue
			}
			container.AddContainerDependency(scContainer.Name, ContainerOrderingHealthyCondition)
			scContainer.BuildContainerDependency(container.Name, apicontainerstatus.ContainerStopped, apicontainerstatus.ContainerStopped)
		}
	}

	// Handle ordering for Volumes
	for _, container := range task.Containers {
		if len(container.VolumesFrom) > 0 {
			for _, volume := range container.VolumesFrom {
				if _, ok := task.ContainerByName(volume.SourceContainer); !ok {
					return fmt.Errorf("could not find volume source container with name %s", volume.SourceContainer)
				}
				dependOn := apicontainer.DependsOn{ContainerName: volume.SourceContainer, Condition: ContainerOrderingCreateCondition}
				container.SetDependsOn(append(container.GetDependsOn(), dependOn))
			}
		}
	}

	// Handle ordering for Links
	for _, container := range task.Containers {
		if len(container.Links) > 0 {
			for _, link := range container.Links {
				linkParts := strings.Split(link, ":")
				if len(linkParts) > 2 {
					return fmt.Errorf("invalid link format")
				}
				linkName := linkParts[0]
				if _, ok := task.ContainerByName(linkName); !ok {
					return fmt.Errorf("could not find container for link %s", link)
				}
				dependOn := apicontainer.DependsOn{ContainerName: linkName, Condition: ContainerOrderingStartCondition}
				container.SetDependsOn(append(container.GetDependsOn(), dependOn))
			}
		}
	}
	return nil
}

func (task *Task) dockerLinks(container *apicontainer.Container, dockerContainerMap map[string]*apicontainer.DockerContainer) ([]string, error) {
	dockerLinkArr := make([]string, len(container.Links))
	for i, link := range container.Links {
		linkParts := strings.Split(link, ":")
		if len(linkParts) > 2 {
			return []string{}, errors.New("Invalid link format")
		}
		linkName := linkParts[0]
		var linkAlias string

		if len(linkParts) == 2 {
			linkAlias = linkParts[1]
		} else {
			logger.Warn("Link name found with no linkalias for container", logger.Fields{
				field.TaskID:    task.GetID(),
				"link":          linkName,
				field.Container: container.Name,
			})
			linkAlias = linkName
		}

		targetContainer, ok := dockerContainerMap[linkName]
		if !ok {
			return []string{}, errors.New("Link target not available: " + linkName)
		}
		dockerLinkArr[i] = targetContainer.DockerName + ":" + linkAlias
	}
	return dockerLinkArr, nil
}

var getHostPortRange = utils.GetHostPortRange
var getHostPort = utils.GetHostPort

// In buildPortMapWithSCIngressConfig, the dockerPortMap and the containerPortSet will be constructed
// for ingress listeners under two service connect bridge mode cases:
// (1) non-default bridge mode service connect experience: customers specify host ports for listeners in the ingress config.
// (2) default bridge mode service connect experience: customers do not specify host ports for listeners in the ingress config.
//
//	Instead, ECS Agent finds host ports within the given dynamic host port range. An error will be returned for case (2) if
//	ECS Agent cannot find an available host port within range.
func (task *Task) buildPortMapWithSCIngressConfig(dynamicHostPortRange string) (nat.PortMap, error) {
	var err error
	ingressDockerPortMap := nat.PortMap{}
	ingressContainerPortSet := make(map[int]struct{})
	protocolStr := "tcp"
	scContainer := task.GetServiceConnectContainer()
	for _, ic := range task.ServiceConnectConfig.IngressConfig {
		listenerPortInt := int(ic.ListenerPort)
		dockerPort := nat.Port(strconv.Itoa(listenerPortInt) + "/" + protocolStr)
		hostPortStr := ""
		if ic.HostPort != nil {
			// For non-default bridge mode service connect experience, a host port is specified by customers
			// Note that service connect ingress config has been validated in service_connect_validator.go,
			// where host ports will be validated to ensure user-defined ports are within a valid port range (1 to 65535)
			// and do not have port collisions.
			hostPortStr = strconv.Itoa(int(*ic.HostPort))
		} else {
			// For default bridge mode service connect experience, customers do not specify a host port
			// thus the host port will be assigned by ECS Agent.
			// ECS Agent will find an available host port within the given dynamic host port range,
			// or return an error if no host port is available within the range.
			hostPortStr, err = getHostPort(protocolStr, dynamicHostPortRange)
			if err != nil {
				return nil, err
			}
		}

		ingressDockerPortMap[dockerPort] = append(ingressDockerPortMap[dockerPort], nat.PortBinding{HostPort: hostPortStr})
		// Append non-range, singular container port to the ingressContainerPortSet
		ingressContainerPortSet[listenerPortInt] = struct{}{}
		// Set taskContainer.ContainerPortSet to be used during network binding creation
		scContainer.SetContainerPortSet(ingressContainerPortSet)
	}
	return ingressDockerPortMap, err
}

// dockerPortMap creates a port binding map for
// (1) Ingress listeners for the service connect AppNet container in the service connect bridge network mode task.
// (2) Port mapping configured by customers in the task definition.
//
// For service connect bridge mode task, we will create port bindings for customers' application containers
// and service connect AppNet container, and let them be published by the associated pause containers.
// (a) For default bridge service connect experience, ECS Agent will assign a host port within the
// default/user-specified dynamic host port range for the ingress listener. If no available host port can be
// found by ECS Agent, an error will be returned.
// (b) For non-default bridge service connect experience, ECS Agent will use the user-defined host port for the ingress listener.
//
// For non-service connect bridge network mode task, ECS Agent will assign a host port or a host port range
// within the default/user-specified dynamic host port range. If no available host port or host port range can be
// found by ECS Agent, an error will be returned.
//
// Note that
// (a) ECS Agent will not assign a new host port within the dynamic host port range for awsvpc network mode task
// (b) ECS Agent will not assign a new host port within the dynamic host port range if the user-specified host port exists
func (task *Task) dockerPortMap(container *apicontainer.Container, dynamicHostPortRange string) (nat.PortMap, error) {
	hostPortStr := ""
	dockerPortMap := nat.PortMap{}
	containerToCheck := container
	containerPortSet := make(map[int]struct{})
	containerPortRangeMap := make(map[string]string)

	// For service connect bridge network mode task, we will create port bindings for task containers,
	// including both application containers and service connect AppNet container, and let them be published
	// by the associated pause containers.
	if task.IsServiceConnectEnabled() && task.IsNetworkModeBridge() {
		if container.Type == apicontainer.ContainerCNIPause {
			// Find the task container associated with this particular pause container
			taskContainer, err := task.getBridgeModeTaskContainerForPauseContainer(container)
			if err != nil {
				return nil, err
			}

			scContainer := task.GetServiceConnectContainer()
			if taskContainer == scContainer {
				// If the associated task container to this pause container is the service connect AppNet container,
				// create port binding(s) for ingress listener ports based on its ingress config.
				// Note that there is no need to do this for egress listener ports as they won't be accessed
				// from host level or from outside.
				dockerPortMap, err := task.buildPortMapWithSCIngressConfig(dynamicHostPortRange)
				if err != nil {
					logger.Error("Failed to build a port map with service connect ingress config", logger.Fields{
						field.TaskID:           task.GetID(),
						field.Container:        taskContainer.Name,
						"dynamicHostPortRange": dynamicHostPortRange,
						field.Error:            err,
					})
					return nil, err
				}
				return dockerPortMap, nil
			}
			// If the associated task container to this pause container is NOT the service connect AppNet container,
			// we will continue to update the dockerPortMap for the pause container using the port bindings
			// configured for the application container since port bindings will be published by the pause container.
			containerToCheck = taskContainer
		} else {
			// If the container is not a pause container, then it is a regular customers' application container
			// or a service connect AppNet container. We will leave the map empty and return it as its port bindings(s)
			// are published by the associated pause container.
			return dockerPortMap, nil
		}
	}

	// For each port binding config, either one of containerPort or containerPortRange is set.
	// (1) containerPort is the port number on the container that's bound to the user-specified host port or the
	//     host port assigned by ECS Agent.
	// (2) containerPortRange is the port number range on the container that's bound to the mapped host port range
	//     found by ECS Agent.
	var err error
	for _, portBinding := range containerToCheck.Ports {
		if portBinding.ContainerPort != 0 {
			containerPort := int(portBinding.ContainerPort)
			protocolStr := portBinding.Protocol.String()
			dockerPort := nat.Port(strconv.Itoa(containerPort) + "/" + protocolStr)

			if portBinding.HostPort != 0 {
				// A user-specified host port exists.
				// Note that the host port value has been validated by ECS front end service;
				// thus only a valid host port value will be streamed down to ECS Agent.
				hostPortStr = strconv.Itoa(int(portBinding.HostPort))
			} else {
				// If there is no user-specified host port, ECS Agent will find an available host port
				// within the given dynamic host port range. And if no host port is available within the range,
				// an error will be returned.
				logger.Debug("No user-specified host port, ECS Agent will find an available host port within the given dynamic host port range", logger.Fields{
					field.Container:        containerToCheck.Name,
					"dynamicHostPortRange": dynamicHostPortRange,
				})
				hostPortStr, err = getHostPort(protocolStr, dynamicHostPortRange)
				if err != nil {
					logger.Error("Unable to find a host port for container within the given dynamic host port range", logger.Fields{
						field.TaskID:           task.GetID(),
						field.Container:        container.Name,
						"dynamicHostPortRange": dynamicHostPortRange,
						field.Error:            err,
					})
					return nil, err
				}
			}
			dockerPortMap[dockerPort] = append(dockerPortMap[dockerPort], nat.PortBinding{HostPort: hostPortStr})

			// For the containerPort case, append a non-range, singular container port to the containerPortSet.
			containerPortSet[containerPort] = struct{}{}
		} else if portBinding.ContainerPortRange != "" {
			containerToCheck.SetContainerHasPortRange(true)

			containerPortRange := portBinding.ContainerPortRange
			// nat.ParsePortRangeToInt validates a port range; if valid, it returns start and end ports as integers.
			startContainerPort, endContainerPort, err := nat.ParsePortRangeToInt(containerPortRange)
			if err != nil {
				return nil, err
			}

			numberOfPorts := endContainerPort - startContainerPort + 1
			protocol := portBinding.Protocol.String()
			// We will try to get a contiguous set of host ports from the ephemeral host port range.
			// This is to ensure that docker maps host ports in a contiguous manner, and
			// we are guaranteed to have the entire hostPortRange in a single network binding while sending this info to ECS;
			// therefore, an error will be returned if we cannot find a contiguous set of host ports.
			hostPortRange, err := getHostPortRange(numberOfPorts, protocol, dynamicHostPortRange)
			if err != nil {
				logger.Error("Unable to find contiguous host ports for container", logger.Fields{
					field.TaskID:         task.GetID(),
					field.Container:      container.Name,
					"containerPortRange": containerPortRange,
					field.Error:          err,
				})
				return nil, err
			}

			// For the ContainerPortRange case, append ranges to the dockerPortMap.
			// nat.ParsePortSpec returns a list of port mappings in a format that Docker likes.
			mappings, err := nat.ParsePortSpec(hostPortRange + ":" + containerPortRange + "/" + protocol)
			if err != nil {
				return nil, err
			}

			for _, mapping := range mappings {
				dockerPortMap[mapping.Port] = append(dockerPortMap[mapping.Port], mapping.Binding)
			}

			// For the ContainerPortRange case, append containerPortRange and associated hostPortRange to the containerPortRangeMap.
			// This will ensure that we consolidate range into 1 network binding while sending it to ECS.
			containerPortRangeMap[containerPortRange] = hostPortRange
		}
	}

	// Set Container.ContainerPortSet and Container.ContainerPortRangeMap to be used during network binding creation.
	containerToCheck.SetContainerPortSet(containerPortSet)
	containerToCheck.SetContainerPortRangeMap(containerPortRangeMap)
	return dockerPortMap, nil
}

func (task *Task) dockerVolumesFrom(container *apicontainer.Container,
	dockerContainerMap map[string]*apicontainer.DockerContainer) ([]string, error) {
	volumesFrom := make([]string, len(container.VolumesFrom))
	for i, volume := range container.VolumesFrom {
		targetContainer, ok := dockerContainerMap[volume.SourceContainer]
		if !ok {
			return []string{}, errors.New("Volume target not available: " + volume.SourceContainer)
		}
		if volume.ReadOnly {
			volumesFrom[i] = targetContainer.DockerName + ":ro"
		} else {
			volumesFrom[i] = targetContainer.DockerName
		}
	}
	return volumesFrom, nil
}

func (task *Task) dockerHostBinds(container *apicontainer.Container) ([]string, error) {
	if container.Name == emptyHostVolumeName {
		// emptyHostVolumes are handled as a special case in config, not
		// hostConfig
		return []string{}, nil
	}

	binds := make([]string, len(container.MountPoints))
	for i, mountPoint := range container.MountPoints {
		hv, ok := task.HostVolumeByName(mountPoint.SourceVolume)
		if !ok {
			return []string{}, errors.New("Invalid volume referenced: " + mountPoint.SourceVolume)
		}

		if hv.Source() == "" || mountPoint.ContainerPath == "" {
			logger.Error("Unable to resolve volume mounts for container; invalid path", logger.Fields{
				field.TaskID:    task.GetID(),
				field.Container: container.Name,
				field.Volume:    mountPoint.SourceVolume,
				"path":          hv.Source(),
				"containerPath": mountPoint.ContainerPath,
			})
			return []string{}, errors.Errorf("Unable to resolve volume mounts; invalid path: %s %s; %s -> %s",
				container.Name, mountPoint.SourceVolume, hv.Source(), mountPoint.ContainerPath)
		}

		bind := hv.Source() + ":" + mountPoint.ContainerPath
		if mountPoint.ReadOnly {
			bind += ":ro"
		}
		binds[i] = bind
	}

	return binds, nil
}

// UpdateStatus updates a task's known and desired statuses to be compatible with all of its containers.
// It will return a bool indicating if there was a change in the task's known status.
func (task *Task) UpdateStatus() bool {
	change := task.updateTaskKnownStatus()
	if change != apitaskstatus.TaskStatusNone {
		logger.Debug("Task known status change", task.fieldsUnsafe())
	}
	// DesiredStatus can change based on a new known status
	task.UpdateDesiredStatus()
	return change != apitaskstatus.TaskStatusNone
}

// UpdateDesiredStatus sets the desired status of the task, and its containers and resources.
func (task *Task) UpdateDesiredStatus() {
	task.lock.Lock()
	defer task.lock.Unlock()
	task.updateTaskDesiredStatusUnsafe()
	task.updateContainerDesiredStatusUnsafe(task.DesiredStatusUnsafe)
	task.updateResourceDesiredStatusUnsafe(task.DesiredStatusUnsafe)
}

// updateTaskDesiredStatusUnsafe determines what status the task should properly be at based on the containers' statuses
// Invariant: task desired status must be stopped if any essential container is stopped
func (task *Task) updateTaskDesiredStatusUnsafe() {
	logger.Debug("Updating task's desired status", task.fieldsUnsafe())

	// A task's desired status is stopped if any essential container is stopped
	// Otherwise, the task's desired status is unchanged (typically running, but no need to change)
	for _, cont := range task.Containers {
		if task.DesiredStatusUnsafe == apitaskstatus.TaskStopped {
			break
		}
		if cont.Essential && (cont.KnownTerminal() || cont.DesiredTerminal()) {
			task.DesiredStatusUnsafe = apitaskstatus.TaskStopped
			logger.Info("Essential container stopped; updated task desired status to stopped",
				task.fieldsUnsafe())
		}
	}
}

// updateContainerDesiredStatusUnsafe sets all containers' desired statuses to the task's desired status
// Invariant: container desired status is <= task desired status converted to container status
// Note: task desired status and container desired status is typically only RUNNING or STOPPED
func (task *Task) updateContainerDesiredStatusUnsafe(taskDesiredStatus apitaskstatus.TaskStatus) {
	for _, container := range task.Containers {
		taskDesiredStatusToContainerStatus := apitaskstatus.MapTaskToContainerStatus(taskDesiredStatus,
			container.GetSteadyStateStatus())
		if container.GetDesiredStatus() < taskDesiredStatusToContainerStatus {
			container.SetDesiredStatus(taskDesiredStatusToContainerStatus)
		}
	}
}

// updateResourceDesiredStatusUnsafe sets all resources' desired status depending on the task's desired status
// TODO: Create a mapping of resource status to the corresponding task status and use it here
func (task *Task) updateResourceDesiredStatusUnsafe(taskDesiredStatus apitaskstatus.TaskStatus) {
	resources := task.getResourcesUnsafe()
	for _, r := range resources {
		if taskDesiredStatus == apitaskstatus.TaskRunning {
			if r.GetDesiredStatus() < r.SteadyState() {
				r.SetDesiredStatus(r.SteadyState())
			}
		} else {
			if r.GetDesiredStatus() < r.TerminalStatus() {
				r.SetDesiredStatus(r.TerminalStatus())
			}
		}
	}
}

// SetKnownStatus sets the known status of the task
func (task *Task) SetKnownStatus(status apitaskstatus.TaskStatus) {
	task.setKnownStatus(status)
	task.updateKnownStatusTime()
}

func (task *Task) setKnownStatus(status apitaskstatus.TaskStatus) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.KnownStatusUnsafe = status
}

func (task *Task) updateKnownStatusTime() {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.KnownStatusTimeUnsafe = ttime.Now()
}

// GetKnownStatus gets the KnownStatus of the task
func (task *Task) GetKnownStatus() apitaskstatus.TaskStatus {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.KnownStatusUnsafe
}

// GetKnownStatusTime gets the KnownStatusTime of the task
func (task *Task) GetKnownStatusTime() time.Time {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.KnownStatusTimeUnsafe
}

// SetCredentialsID sets the credentials ID for the task
func (task *Task) SetCredentialsID(id string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.credentialsID = id
}

// GetCredentialsID gets the credentials ID for the task
func (task *Task) GetCredentialsID() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.credentialsID
}

// SetCredentialsRelativeURI sets the credentials relative uri for the task
func (task *Task) SetCredentialsRelativeURI(uri string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.credentialsRelativeURIUnsafe = uri
}

// GetCredentialsRelativeURI returns the credentials relative uri for the task
func (task *Task) GetCredentialsRelativeURI() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.credentialsRelativeURIUnsafe
}

// SetExecutionRoleCredentialsID sets the ID for the task execution role credentials
func (task *Task) SetExecutionRoleCredentialsID(id string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.ExecutionCredentialsID = id
}

// GetExecutionCredentialsID gets the credentials ID for the task
func (task *Task) GetExecutionCredentialsID() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.ExecutionCredentialsID
}

// GetDesiredStatus gets the desired status of the task
func (task *Task) GetDesiredStatus() apitaskstatus.TaskStatus {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.DesiredStatusUnsafe
}

// SetDesiredStatus sets the desired status of the task
func (task *Task) SetDesiredStatus(status apitaskstatus.TaskStatus) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.DesiredStatusUnsafe = status
}

// GetSentStatus safely returns the SentStatus of the task
func (task *Task) GetSentStatus() apitaskstatus.TaskStatus {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.SentStatusUnsafe
}

// SetSentStatus safely sets the SentStatus of the task
func (task *Task) SetSentStatus(status apitaskstatus.TaskStatus) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.SentStatusUnsafe = status
}

// AddTaskENI adds ENI information to the task.
func (task *Task) AddTaskENI(eni *ni.NetworkInterface) {
	task.lock.Lock()
	defer task.lock.Unlock()

	if task.ENIs == nil {
		task.ENIs = make([]*ni.NetworkInterface, 0)
	}
	task.ENIs = append(task.ENIs, eni)
}

// GetTaskENIs returns the list of ENIs for the task.
func (task *Task) GetTaskENIs() []*ni.NetworkInterface {
	// TODO: what's the point of locking if we are returning a pointer?
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.ENIs
}

// GetPrimaryENI returns the primary ENI of the task. Since ACS can potentially send
// multiple ENIs to the agent, the first ENI in the list is considered as the primary ENI.
func (task *Task) GetPrimaryENI() *ni.NetworkInterface {
	task.lock.RLock()
	defer task.lock.RUnlock()

	if len(task.ENIs) == 0 {
		return nil
	}

	return task.ENIs[0]
}

// SetAppMesh sets the app mesh config of the task
func (task *Task) SetAppMesh(appMesh *nlappmesh.AppMesh) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.AppMesh = appMesh
}

// GetAppMesh returns the app mesh config of the task
func (task *Task) GetAppMesh() *nlappmesh.AppMesh {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.AppMesh
}

// SetPullStartedAt sets the task pullstartedat timestamp and returns whether
// this field was updated or not
func (task *Task) SetPullStartedAt(timestamp time.Time) bool {
	task.lock.Lock()
	defer task.lock.Unlock()

	// Only set this field if it is not set
	if task.PullStartedAtUnsafe.IsZero() {
		task.PullStartedAtUnsafe = timestamp
		return true
	}
	return false
}

// GetPullStartedAt returns the PullStartedAt timestamp
func (task *Task) GetPullStartedAt() time.Time {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.PullStartedAtUnsafe
}

// SetPullStoppedAt sets the task pullstoppedat timestamp
func (task *Task) SetPullStoppedAt(timestamp time.Time) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.PullStoppedAtUnsafe = timestamp
}

// GetPullStoppedAt returns the PullStoppedAt timestamp
func (task *Task) GetPullStoppedAt() time.Time {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.PullStoppedAtUnsafe
}

// SetExecutionStoppedAt sets the ExecutionStoppedAt timestamp of the task
func (task *Task) SetExecutionStoppedAt(timestamp time.Time) bool {
	task.lock.Lock()
	defer task.lock.Unlock()

	if task.ExecutionStoppedAtUnsafe.IsZero() {
		task.ExecutionStoppedAtUnsafe = timestamp
		return true
	}
	return false
}

// GetExecutionStoppedAt returns the task executionStoppedAt timestamp
func (task *Task) GetExecutionStoppedAt() time.Time {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.ExecutionStoppedAtUnsafe
}

// String returns a human-readable string representation of this object
func (task *Task) String() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.stringUnsafe()
}

// stringUnsafe returns a human-readable string representation of this object
func (task *Task) stringUnsafe() string {
	return fmt.Sprintf("%s:%s %s, TaskStatus: (%s->%s) N Containers: %d, N ENIs %d",
		task.Family, task.Version, task.Arn,
		task.KnownStatusUnsafe.String(), task.DesiredStatusUnsafe.String(),
		len(task.Containers), len(task.ENIs))
}

// fieldsUnsafe returns logger.Fields representing this object
func (task *Task) fieldsUnsafe() logger.Fields {
	return logger.Fields{
		"taskFamily":        task.Family,
		"taskVersion":       task.Version,
		"taskArn":           task.Arn,
		"taskKnownStatus":   task.KnownStatusUnsafe.String(),
		"taskDesiredStatus": task.DesiredStatusUnsafe.String(),
		"nContainers":       len(task.Containers),
		"nENIs":             len(task.ENIs),
	}
}

// GetID is used to retrieve the taskID from taskARN
// Reference: http://docs.aws.amazon.com/general/latest/gr/aws-arns-and-namespaces.html#arn-syntax-ecs
func (task *Task) GetID() string {
	task.setIdOnce.Do(func() {
		id, err := arn.TaskIdFromArn(task.Arn)
		if err != nil {
			logger.Error("Error getting ID for task", logger.Fields{
				field.TaskARN: task.Arn,
				field.Error:   err,
			})
		}
		task.id = id
	})

	return task.id
}

// RecordExecutionStoppedAt checks if this is an essential container stopped
// and set the task executionStoppedAt timestamps
func (task *Task) RecordExecutionStoppedAt(container *apicontainer.Container) {
	if !container.Essential {
		return
	}
	if container.GetKnownStatus() != apicontainerstatus.ContainerStopped {
		return
	}
	// If the essential container is stopped, set the ExecutionStoppedAt timestamp
	now := time.Now()
	ok := task.SetExecutionStoppedAt(now)
	if !ok {
		// ExecutionStoppedAt was already recorded. Nothing to left to do here
		return
	}
	logger.Info("Essential container stopped; recording task stopped time", logger.Fields{
		field.TaskID:             task.GetID(),
		field.Container:          container.Name,
		field.ExecutionStoppedAt: now.String(),
	})
}

// GetResources returns the list of task resources from ResourcesMap
func (task *Task) GetResources() []taskresource.TaskResource {
	task.lock.RLock()
	defer task.lock.RUnlock()
	return task.getResourcesUnsafe()
}

// getResourcesUnsafe returns the list of task resources from ResourcesMap
func (task *Task) getResourcesUnsafe() []taskresource.TaskResource {
	var resourceList []taskresource.TaskResource
	for _, resources := range task.ResourcesMapUnsafe {
		resourceList = append(resourceList, resources...)
	}
	return resourceList
}

// AddResource adds a resource to ResourcesMap
func (task *Task) AddResource(resourceType string, resource taskresource.TaskResource) {
	task.lock.Lock()
	defer task.lock.Unlock()
	task.ResourcesMapUnsafe[resourceType] = append(task.ResourcesMapUnsafe[resourceType], resource)
}

// requiresAnyCredentialSpecResource returns true if at least one container in the task
// needs a valid credentialspec resource (domain-joined or domainless)
func (task *Task) requiresAnyCredentialSpecResource() bool {
	for _, container := range task.Containers {
		if container.RequiresAnyCredentialSpec() {
			return true
		}
	}
	return false
}

// RequiresDomainlessCredentialSpecResource returns true if at least one container in the task
// needs a valid domainless credentialspec resource
func (task *Task) RequiresDomainlessCredentialSpecResource() bool {
	for _, container := range task.Containers {
		if container.RequiresDomainlessCredentialSpec() {
			return true
		}
	}
	return false
}

// GetCredentialSpecResource retrieves credentialspec resource from resource map
func (task *Task) GetCredentialSpecResource() ([]taskresource.TaskResource, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	res, ok := task.ResourcesMapUnsafe[credentialspec.ResourceName]
	return res, ok
}

// GetAllCredentialSpecRequirements is used to build all the credential spec requirements for the task
func (task *Task) GetAllCredentialSpecRequirements() map[string]string {
	reqsContainerMap := make(map[string]string)
	for _, container := range task.Containers {
		credentialSpec, err := container.GetCredentialSpec()
		if err == nil && credentialSpec != "" {
			reqsContainerMap[credentialSpec] = container.Name
		}
	}
	return reqsContainerMap
}

// SetTerminalReason sets the terminalReason string and this can only be set
// once per the task's lifecycle. This field does not accept updates.
func (task *Task) SetTerminalReason(reason string) {
	task.terminalReasonOnce.Do(func() {
		logger.Info("Setting terminal reason for task", logger.Fields{
			field.TaskID: task.GetID(),
			field.Reason: reason,
		})
		// Converts the first letter of terminal reason into capital letter
		words := strings.Fields(reason)
		words[0] = strings.Title(words[0])
		task.terminalReason = strings.Join(words, " ")
	})
}

// GetTerminalReason retrieves the terminalReason string
func (task *Task) GetTerminalReason() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.terminalReason
}

// PopulateASMAuthData sets docker auth credentials for a container
func (task *Task) PopulateASMAuthData(container *apicontainer.Container) error {
	secretID := container.RegistryAuthentication.ASMAuthData.CredentialsParameter
	resource, ok := task.getASMAuthResource()
	if !ok {
		return errors.New("task auth data: unable to fetch ASM resource")
	}
	// This will cause a panic if the resource is not of ASMAuthResource type.
	// But, it's better to panic as we should have never reached condition
	// unless we released an agent without any testing around that code path
	asmResource := resource[0].(*asmauth.ASMAuthResource)
	dac, ok := asmResource.GetASMDockerAuthConfig(secretID)
	if !ok {
		return errors.Errorf("task auth data: unable to fetch docker auth config [%s]", secretID)
	}
	container.SetASMDockerAuthConfig(dac)
	return nil
}

func (task *Task) getASMAuthResource() ([]taskresource.TaskResource, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	res, ok := task.ResourcesMapUnsafe[asmauth.ResourceName]
	return res, ok
}

// getSSMSecretsResource retrieves ssmsecret resource from resource map
func (task *Task) getSSMSecretsResource() ([]taskresource.TaskResource, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	res, ok := task.ResourcesMapUnsafe[ssmsecret.ResourceName]
	return res, ok
}

// PopulateSecrets appends secrets to container's env var map and hostconfig section
func (task *Task) PopulateSecrets(hostConfig *dockercontainer.HostConfig, container *apicontainer.Container) *apierrors.DockerClientConfigError {
	var ssmRes *ssmsecret.SSMSecretResource
	var asmRes *asmsecret.ASMSecretResource

	if container.ShouldCreateWithSSMSecret() {
		resource, ok := task.getSSMSecretsResource()
		if !ok {
			return &apierrors.DockerClientConfigError{Msg: "task secret data: unable to fetch SSM Secrets resource"}
		}
		ssmRes = resource[0].(*ssmsecret.SSMSecretResource)
	}

	if container.ShouldCreateWithASMSecret() {
		resource, ok := task.getASMSecretsResource()
		if !ok {
			return &apierrors.DockerClientConfigError{Msg: "task secret data: unable to fetch ASM Secrets resource"}
		}
		asmRes = resource[0].(*asmsecret.ASMSecretResource)
	}

	populateContainerSecrets(hostConfig, container, ssmRes, asmRes)
	return nil
}

func populateContainerSecrets(hostConfig *dockercontainer.HostConfig, container *apicontainer.Container,
	ssmRes *ssmsecret.SSMSecretResource, asmRes *asmsecret.ASMSecretResource) {
	envVars := make(map[string]string)

	logDriverTokenName := ""
	logDriverTokenSecretValue := ""

	for _, secret := range container.Secrets {
		secretVal := ""

		if secret.Provider == apicontainer.SecretProviderSSM {
			k := secret.GetSecretResourceCacheKey()
			if secretValue, ok := ssmRes.GetCachedSecretValue(k); ok {
				secretVal = secretValue
			}
		}

		if secret.Provider == apicontainer.SecretProviderASM {
			k := secret.GetSecretResourceCacheKey()
			if secretValue, ok := asmRes.GetCachedSecretValue(k); ok {
				secretVal = secretValue
			}
		}

		if secret.Type == apicontainer.SecretTypeEnv {
			envVars[secret.Name] = secretVal
			continue
		}

		if secret.Target == apicontainer.SecretTargetLogDriver {
			// Log driver secrets for container using awsfirelens log driver won't be saved in log config and passed to
			// Docker here. They will only be used to configure the firelens container.
			if container.GetLogDriver() == firelensDriverName {
				continue
			}

			logDriverTokenName = secret.Name
			logDriverTokenSecretValue = secretVal

			// Check if all the name and secret value for the log driver do exist
			// And add the secret value for this log driver into container's HostConfig
			if hostConfig.LogConfig.Type != "" && logDriverTokenName != "" && logDriverTokenSecretValue != "" {
				if hostConfig.LogConfig.Config == nil {
					hostConfig.LogConfig.Config = map[string]string{}
				}
				hostConfig.LogConfig.Config[logDriverTokenName] = logDriverTokenSecretValue
			}
		}
	}

	container.MergeEnvironmentVariables(envVars)
}

// PopulateSecretLogOptionsToFirelensContainer collects secret log option values for awsfirelens log driver from task
// resource and specified then as envs of firelens container. Firelens container will use the envs to resolve config
// file variables constructed for secret log options when loading the config file.
func (task *Task) PopulateSecretLogOptionsToFirelensContainer(firelensContainer *apicontainer.Container) *apierrors.DockerClientConfigError {
	firelensENVs := make(map[string]string)

	var ssmRes *ssmsecret.SSMSecretResource
	var asmRes *asmsecret.ASMSecretResource

	resource, ok := task.getSSMSecretsResource()
	if ok {
		ssmRes = resource[0].(*ssmsecret.SSMSecretResource)
	}

	resource, ok = task.getASMSecretsResource()
	if ok {
		asmRes = resource[0].(*asmsecret.ASMSecretResource)
	}

	for _, container := range task.Containers {
		if container.GetLogDriver() != firelensDriverName {
			continue
		}

		logDriverSecretData, err := collectLogDriverSecretData(container.Secrets, ssmRes, asmRes)
		if err != nil {
			return &apierrors.DockerClientConfigError{
				Msg: fmt.Sprintf("unable to generate config to create firelens container: %v", err),
			}
		}

		idx := task.GetContainerIndex(container.Name)
		if idx < 0 {
			return &apierrors.DockerClientConfigError{
				Msg: fmt.Sprintf("unable to generate config to create firelens container because container %s is not found in task", container.Name),
			}
		}
		for key, value := range logDriverSecretData {
			envKey := fmt.Sprintf(firelensConfigVarFmt, key, idx)
			firelensENVs[envKey] = value
		}
	}

	firelensContainer.MergeEnvironmentVariables(firelensENVs)
	return nil
}

// collectLogDriverSecretData collects all the secret values for log driver secrets.
func collectLogDriverSecretData(secrets []apicontainer.Secret, ssmRes *ssmsecret.SSMSecretResource,
	asmRes *asmsecret.ASMSecretResource) (map[string]string, error) {
	secretData := make(map[string]string)
	for _, secret := range secrets {
		if secret.Target != apicontainer.SecretTargetLogDriver {
			continue
		}

		secretVal := ""
		cacheKey := secret.GetSecretResourceCacheKey()
		if secret.Provider == apicontainer.SecretProviderSSM {
			if ssmRes == nil {
				return nil, errors.Errorf("missing secret value for secret %s", secret.Name)
			}

			if secretValue, ok := ssmRes.GetCachedSecretValue(cacheKey); ok {
				secretVal = secretValue
			}
		} else if secret.Provider == apicontainer.SecretProviderASM {
			if asmRes == nil {
				return nil, errors.Errorf("missing secret value for secret %s", secret.Name)
			}

			if secretValue, ok := asmRes.GetCachedSecretValue(cacheKey); ok {
				secretVal = secretValue
			}
		}

		secretData[secret.Name] = secretVal
	}

	return secretData, nil
}

// getASMSecretsResource retrieves asmsecret resource from resource map
func (task *Task) getASMSecretsResource() ([]taskresource.TaskResource, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	res, ok := task.ResourcesMapUnsafe[asmsecret.ResourceName]
	return res, ok
}

// InitializeResources initializes the required field in the task on agent restart
// Some of the fields in task isn't saved in the agent state file, agent needs
// to initialize these fields before processing the task, eg: docker client in resource
func (task *Task) InitializeResources(config *config.Config, resourceFields *taskresource.ResourceFields) {
	task.lock.Lock()
	defer task.lock.Unlock()

	for _, resources := range task.ResourcesMapUnsafe {
		for _, resource := range resources {
			resource.Initialize(config, resourceFields, task.KnownStatusUnsafe, task.DesiredStatusUnsafe)
		}
	}
}

// Retrieves a Task's PIDMode
func (task *Task) getPIDMode() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.PIDMode
}

// Retrieves a Task's IPCMode
func (task *Task) getIPCMode() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.IPCMode
}

// AssociationsByTypeAndContainer gets a list of names of all the associations associated with a container and of a
// certain type
func (task *Task) AssociationsByTypeAndContainer(associationType, containerName string) []string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	var associationNames []string
	for _, association := range task.Associations {
		if association.Type == associationType {
			for _, associatedContainerName := range association.Containers {
				if associatedContainerName == containerName {
					associationNames = append(associationNames, association.Name)
				}
			}
		}
	}

	return associationNames
}

// AssociationByTypeAndName gets an association of a certain type and name
func (task *Task) AssociationByTypeAndName(associationType, associationName string) (*Association, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	for _, association := range task.Associations {
		if association.Type == associationType && association.Name == associationName {
			return &association, true
		}
	}

	return nil, false
}

// GetContainerIndex returns the index of the container in the container list. This doesn't count internal container.
func (task *Task) GetContainerIndex(containerName string) int {
	task.lock.RLock()
	defer task.lock.RUnlock()

	idx := 0
	for _, container := range task.Containers {
		if container.IsInternal() {
			continue
		}
		if container.Name == containerName {
			return idx
		}
		idx++
	}
	return -1
}

func (task *Task) initializeEnvfilesResource(config *config.Config, credentialsManager credentials.Manager) error {

	for _, container := range task.Containers {
		if container.ShouldCreateWithEnvFiles() {
			envfileResource, err := envFiles.NewEnvironmentFileResource(config.Cluster, task.Arn, config.AWSRegion, config.DataDir,
				container.Name, container.EnvironmentFiles, credentialsManager, task.ExecutionCredentialsID, config.InstanceIPCompatibility)
			if err != nil {
				return errors.Wrapf(err, "unable to initialize envfiles resource for container %s", container.Name)
			}
			task.AddResource(envFiles.ResourceName, envfileResource)
			container.BuildResourceDependency(envfileResource.GetName(), resourcestatus.ResourceStatus(envFiles.EnvFileCreated), apicontainerstatus.ContainerCreated)
		}
	}

	return nil
}

func (task *Task) getEnvfilesResource(containerName string) (taskresource.TaskResource, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	resources, ok := task.ResourcesMapUnsafe[envFiles.ResourceName]
	if !ok {
		return nil, false
	}

	for _, resource := range resources {
		envfileResource := resource.(*envFiles.EnvironmentFileResource)
		if envfileResource.GetContainerName() == containerName {
			return envfileResource, true
		}
	}

	// was not able to retrieve envfile resource for specified container name
	return nil, false
}

// MergeEnvVarsFromEnvfiles should be called when creating a container -
// this method reads the environment variables specified in the environment files
// that was downloaded to disk and merges it with existing environment variables
func (task *Task) MergeEnvVarsFromEnvfiles(container *apicontainer.Container) *apierrors.ResourceInitError {
	var envfileResource *envFiles.EnvironmentFileResource
	resource, ok := task.getEnvfilesResource(container.Name)
	if !ok {
		err := errors.New(fmt.Sprintf("task environment files: unable to retrieve environment files resource for container %s", container.Name))
		return apierrors.NewResourceInitError(task.Arn, err)
	}
	envfileResource = resource.(*envFiles.EnvironmentFileResource)

	envVarsList, err := envfileResource.ReadEnvVarsFromEnvfiles()
	if err != nil {
		return apierrors.NewResourceInitError(task.Arn, err)
	}

	err = container.MergeEnvironmentVariablesFromEnvfiles(envVarsList)
	if err != nil {
		return apierrors.NewResourceInitError(task.Arn, err)
	}

	return nil
}

// GetLocalIPAddress returns the local IP address of the task.
func (task *Task) GetLocalIPAddress() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.LocalIPAddressUnsafe
}

// SetLocalIPAddress sets the local IP address of the task.
func (task *Task) SetLocalIPAddress(addr string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.LocalIPAddressUnsafe = addr
}

// UpdateTaskENIsLinkName updates the link name of all the enis associated with the task.
func (task *Task) UpdateTaskENIsLinkName() {
	task.lock.Lock()
	defer task.lock.Unlock()

	// Update the link name of the task eni.
	for _, eni := range task.ENIs {
		eni.GetLinkName()
	}
}

func (task *Task) GetServiceConnectContainer() *apicontainer.Container {
	if task.ServiceConnectConfig == nil {
		return nil
	}
	c, _ := task.ContainerByName(task.ServiceConnectConfig.ContainerName)
	return c
}

// IsContainerServiceConnectPause checks whether a given container name is the name of the task service connect pause
// container. We construct the name of SC pause container by taking SC container name from SC config, and using the
// pause container naming pattern.
func (task *Task) IsContainerServiceConnectPause(containerName string) bool {
	scContainer := task.GetServiceConnectContainer()
	if scContainer == nil {
		return false
	}
	scPauseName := fmt.Sprintf(ServiceConnectPauseContainerNameFormat, scContainer.Name)
	return containerName == scPauseName
}

// IsServiceConnectEnabled returns true if Service Connect is enabled for this task.
func (task *Task) IsServiceConnectEnabled() bool {
	return task.GetServiceConnectContainer() != nil
}

// IsEBSTaskAttachEnabled returns true if this task has EBS volume configuration in its ACS payload.
// TODO: as more daemons come online, we'll want a generic handler these bool checks and payload handling
func (task *Task) IsEBSTaskAttachEnabled() bool {
	task.lock.RLock()
	defer task.lock.RUnlock()
	return task.isEBSTaskAttachEnabledUnsafe()
}

func (task *Task) isEBSTaskAttachEnabledUnsafe() bool {
	logger.Debug("Checking if there are any ebs volume configs")
	for _, tv := range task.Volumes {
		switch tv.Volume.(type) {
		case *taskresourcevolume.EBSTaskVolumeConfig:
			logger.Debug("found ebs volume config")
			return true
		default:
			continue
		}
	}
	return false
}

func (task *Task) IsServiceConnectBridgeModeApplicationContainer(container *apicontainer.Container) bool {
	return container.GetNetworkModeFromHostConfig() == "container" && task.IsServiceConnectEnabled()
}

// PopulateServiceConnectContainerMappingEnvVarBridge populates APPNET_CONTAINER_IP_MAPPING
// env var for AppNet Agent container aka SC container for bridge network mode tasks.
func (task *Task) PopulateServiceConnectContainerMappingEnvVarBridge(
	instanceIPCompatibility ipcompatibility.IPCompatibility,
) error {
	containerMapping := make(map[string]string)
	for _, c := range task.Containers {
		if c.Type != apicontainer.ContainerCNIPause {
			continue
		}
		taskContainer, err := task.getBridgeModeTaskContainerForPauseContainer(c)
		if err != nil {
			return fmt.Errorf("error retrieving task container for pause container %s: %+v", c.Name, err)
		}
		if instanceIPCompatibility.IsIPv6Only() {
			if c.GetNetworkSettings().GlobalIPv6Address == "" {
				return fmt.Errorf(
					"instance is IPv6-only but no IPv6 address found for container '%s'",
					taskContainer.Name)
			}
			containerMapping[taskContainer.Name] = c.GetNetworkSettings().GlobalIPv6Address
		} else {
			containerMapping[taskContainer.Name] = c.GetNetworkSettings().IPAddress
		}
	}
	return task.setContainerMappingForServiceConnectContainer(containerMapping)
}

// PopulateServiceConnectContainerMappingEnvVarAwsvpc populates APPNET_CONTAINER_IP_MAPPING
// environment variable for the AppNet Agent container (aka Service Connect container) for
// awsvpc network mode tasks.
// Application containers are expected to be bound to loopback address(es).
func (task *Task) PopulateServiceConnectContainerMappingEnvVarAwsvpc() error {
	primaryENI := task.GetPrimaryENI()
	if primaryENI == nil {
		return errors.New("no primary ENI found in task")
	}
	if !primaryENI.IPv6Only() {
		// Explicit container address mapping is needed only for IPv6-only case.
		// Service Connect sidecar defaults to IPv4 loopback in other cases.
		return nil
	}

	containerMapping := make(map[string]string)
	for _, c := range task.Containers {
		if c.IsInternal() {
			continue
		}
		containerMapping[c.Name] = ipv6LoopbackAddress
	}

	return task.setContainerMappingForServiceConnectContainer(containerMapping)
}

func (task *Task) setContainerMappingForServiceConnectContainer(containerMapping map[string]string) error {
	envVars := make(map[string]string)
	containerMappingJson, err := json.Marshal(containerMapping)
	if err != nil {
		return fmt.Errorf("error injecting required env vars APPNET_CONTAINER_MAPPING to Service Connect container: %w", err)
	}
	envVars[serviceConnectContainerMappingEnvVar] = string(containerMappingJson)
	scContainer := task.GetServiceConnectContainer()
	if scContainer == nil {
		return fmt.Errorf("service connect container not found in task")
	}
	scContainer.MergeEnvironmentVariables(envVars)
	return nil
}

func (task *Task) PopulateServiceConnectRuntimeConfig(serviceConnectConfig serviceconnect.RuntimeConfig) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.ServiceConnectConfig.RuntimeConfig = serviceConnectConfig
}

// PopulateServiceConnectNetworkConfig is called once we've started SC pause container and retrieved its container IPs.
func (task *Task) PopulateServiceConnectNetworkConfig(ipv4Addr string, ipv6Addr string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.ServiceConnectConfig.NetworkConfig = serviceconnect.NetworkConfig{
		SCPauseIPv4Addr: ipv4Addr,
		SCPauseIPv6Addr: ipv6Addr,
	}
}

func (task *Task) GetServiceConnectRuntimeConfig() serviceconnect.RuntimeConfig {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.ServiceConnectConfig.RuntimeConfig
}

func (task *Task) GetServiceConnectNetworkConfig() serviceconnect.NetworkConfig {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.ServiceConnectConfig.NetworkConfig
}

func (task *Task) SetServiceConnectConnectionDraining(draining bool) {
	task.lock.Lock()
	defer task.lock.Unlock()
	task.ServiceConnectConnectionDrainingUnsafe = draining
}

func (task *Task) IsServiceConnectConnectionDraining() bool {
	task.lock.RLock()
	defer task.lock.RUnlock()
	return task.ServiceConnectConnectionDrainingUnsafe
}

func (task *Task) IsLaunchTypeFargate() bool {
	return strings.ToUpper(task.LaunchType) == "FARGATE"
}

// ToHostResources will convert a task to a map of resources which ECS takes into account when scheduling tasks on instances
// * CPU
//   - If task level CPU is set, use that
//   - Else add up container CPUs
//
// * Memory
//   - If task level memory is set, use that
//   - Else add up container level
//   - If memoryReservation field is set, use that
//   - Else use memory field
//
// * Ports (TCP/UDP)
//   - Only account for hostPort
//   - Don't need to account for awsvpc mode, each task gets its own namespace
//
// * GPU
//   - Concatenate each container's gpu ids
func (task *Task) ToHostResources() map[string]ecstypes.Resource {
	resources := make(map[string]ecstypes.Resource)
	// CPU
	if task.CPU > 0 {
		// cpu unit is vcpu at task level
		// convert to cpushares
		taskCPUint32 := int32(task.CPU * 1024)
		resources["CPU"] = ecstypes.Resource{
			Name:         utils.Strptr("CPU"),
			Type:         utils.Strptr("INTEGER"),
			IntegerValue: taskCPUint32,
		}
	} else {
		// cpu unit is cpushares at container level
		containerCPUint32 := int32(0)
		for _, container := range task.Containers {
			containerCPUint32 += int32(container.CPU)
		}
		resources["CPU"] = ecstypes.Resource{
			Name:         utils.Strptr("CPU"),
			Type:         utils.Strptr("INTEGER"),
			IntegerValue: containerCPUint32,
		}
	}

	// Memory
	if task.Memory > 0 {
		// memory unit is MiB at task level
		taskMEMint32 := int32(task.Memory)
		resources["MEMORY"] = ecstypes.Resource{
			Name:         utils.Strptr("MEMORY"),
			Type:         utils.Strptr("INTEGER"),
			IntegerValue: taskMEMint32,
		}
	} else {
		containerMEMint32 := int32(0)

		for _, c := range task.Containers {
			// To parse memory reservation / soft limit
			hostConfig := &dockercontainer.HostConfig{}

			if c.DockerConfig.HostConfig != nil {
				err := json.Unmarshal([]byte(*c.DockerConfig.HostConfig), hostConfig)
				if err != nil || hostConfig.MemoryReservation <= 0 {
					// container memory unit is MiB, keeping as is
					containerMEMint32 += int32(c.Memory)
				} else {
					// Soft limit is specified in MiB units but translated to bytes while being transferred to Agent
					// Converting back to MiB
					containerMEMint32 += int32(hostConfig.MemoryReservation / (1024 * 1024))
				}
			} else {
				// container memory unit is MiB, keeping as is
				containerMEMint32 += int32(c.Memory)
			}
		}
		resources["MEMORY"] = ecstypes.Resource{
			Name:         utils.Strptr("MEMORY"),
			Type:         utils.Strptr("INTEGER"),
			IntegerValue: containerMEMint32,
		}
	}

	// PORTS_TCP and PORTS_UDP
	var tcpPortSet []uint16
	var udpPortSet []uint16

	// AWSVPC tasks have 'host' ports mapped to task ENI, not to host
	// So don't need to keep an 'account' of awsvpc tasks with host ports fields assigned
	if !task.IsNetworkModeAWSVPC() {
		for _, c := range task.Containers {
			for _, port := range c.Ports {
				hostPort := port.HostPort
				protocol := port.Protocol
				if hostPort > 0 && protocol == apicontainer.TransportProtocolTCP {
					tcpPortSet = append(tcpPortSet, hostPort)
				} else if hostPort > 0 && protocol == apicontainer.TransportProtocolUDP {
					udpPortSet = append(udpPortSet, hostPort)
				}
			}
		}
	}
	resources["PORTS_TCP"] = ecstypes.Resource{
		Name:           utils.Strptr("PORTS_TCP"),
		Type:           utils.Strptr("STRINGSET"),
		StringSetValue: commonutils.Uint16SliceToStringSlice(tcpPortSet),
	}
	resources["PORTS_UDP"] = ecstypes.Resource{
		Name:           utils.Strptr("PORTS_UDP"),
		Type:           utils.Strptr("STRINGSET"),
		StringSetValue: commonutils.Uint16SliceToStringSlice(udpPortSet),
	}

	// GPU
	var gpus []string
	for _, c := range task.Containers {
		gpus = append(gpus, c.GPUIDs...)
	}
	resources["GPU"] = ecstypes.Resource{
		Name:           utils.Strptr("GPU"),
		Type:           utils.Strptr("STRINGSET"),
		StringSetValue: gpus,
	}
	logger.Debug("Task host resources to account for", logger.Fields{
		"taskArn":   task.Arn,
		"CPU":       resources["CPU"].IntegerValue,
		"MEMORY":    resources["MEMORY"].IntegerValue,
		"PORTS_TCP": resources["PORTS_TCP"].StringSetValue,
		"PORTS_UDP": resources["PORTS_UDP"].StringSetValue,
		"GPU":       resources["GPU"].StringSetValue,
	})
	return resources
}

func (task *Task) HasActiveContainers() bool {
	for _, container := range task.Containers {
		containerStatus := container.GetKnownStatus()
		if containerStatus >= apicontainerstatus.ContainerManifestPulled &&
			containerStatus <= apicontainerstatus.ContainerResourcesProvisioned {
			return true
		}
	}
	return false
}

// IsManagedDaemonTask will check if a task is a non-stopped managed daemon task
// TODO: Somehow track this on a task level (i.e. obtain the managed daemon image name from task arn and then find the corresponding container with the image name)
func (task *Task) IsManagedDaemonTask() (string, bool) {
	task.lock.RLock()
	defer task.lock.RUnlock()

	// We'll want to obtain the last known non-stopped managed daemon task to be saved into our task engine.
	// There can be an edge case where the task hasn't been progressed to RUNNING yet.
	if !task.IsInternal || task.KnownStatusUnsafe.Terminal() {
		return "", false
	}

	for _, c := range task.Containers {
		if c.IsManagedDaemonContainer() {
			imageName := c.GetImageName()
			return imageName, true
		}
	}
	return "", false
}

func (task *Task) IsRunning() bool {
	task.lock.RLock()
	defer task.lock.RUnlock()
	taskStatus := task.KnownStatusUnsafe

	return taskStatus == apitaskstatus.TaskRunning
}

// HasAContainerWithResolvedDigest checks if the task has at least one container with a successfully
// resolved image manifest digest.
func (task *Task) HasAContainerWithResolvedDigest() bool {
	for _, c := range task.Containers {
		if c.DigestResolved() {
			return true
		}
	}
	return false
}

func (task *Task) IsFaultInjectionEnabled() bool {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.EnableFaultInjection
}

func (task *Task) GetNetworkMode() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.NetworkMode
}

func (task *Task) GetNetworkNamespace() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.NetworkNamespace
}

func (task *Task) SetNetworkNamespace(netNs string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.NetworkNamespace = netNs
}

func (task *Task) GetDefaultIfname() string {
	task.lock.RLock()
	defer task.lock.RUnlock()

	return task.DefaultIfname
}

func (task *Task) SetDefaultIfname(ifname string) {
	task.lock.Lock()
	defer task.lock.Unlock()

	task.DefaultIfname = ifname
}
