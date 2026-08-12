package actions

// Data-layer aliases, mirroring internal/server's store_aliases.go pattern
// (ARCH-001): the engine's moved code keeps its original spellings while the
// types themselves live in internal/store. Only names the engine actually
// uses appear here.

import "github.com/e6qu/bleephub/internal/store"

type (
	Agent               = store.Agent
	CheckRun            = store.CheckRun
	CheckSuite          = store.CheckSuite
	EnvApproval         = store.EnvApproval
	Environment         = store.Environment
	Job                 = store.Job
	JobDef              = store.JobDef
	MatrixDef           = store.MatrixDef
	PendingDeployment   = store.PendingDeployment
	Repo                = store.Repo
	Result              = store.Result
	ServiceDef          = store.ServiceDef
	Session             = store.Session
	StepDef             = store.StepDef
	Store               = store.Store
	TaskAgentMessage    = store.TaskAgentMessage
	User                = store.User
	Workflow            = store.Workflow
	WorkflowCallBinding = store.WorkflowCallBinding
	WorkflowDef         = store.WorkflowDef
	WorkflowInputDef    = store.WorkflowInputDef
	WorkflowJob         = store.WorkflowJob

	runnerScope = store.RunnerScope
)

const (
	WorkflowStatusRunning            = store.WorkflowStatusRunning
	WorkflowStatusCompleted          = store.WorkflowStatusCompleted
	WorkflowStatusPendingConcurrency = store.WorkflowStatusPendingConcurrency
	WorkflowStatusWaiting            = store.WorkflowStatusWaiting
	WorkflowStatusActionRequired     = store.WorkflowStatusActionRequired

	JobStatusPending   = store.JobStatusPending
	JobStatusQueued    = store.JobStatusQueued
	JobStatusRunning   = store.JobStatusRunning
	JobStatusCompleted = store.JobStatusCompleted
	JobStatusSkipped   = store.JobStatusSkipped
	JobStatusWaiting   = store.JobStatusWaiting

	ResultSuccess        = store.ResultSuccess
	ResultFailure        = store.ResultFailure
	ResultCancelled      = store.ResultCancelled
	ResultSkipped        = store.ResultSkipped
	ResultStartupFailure = store.ResultStartupFailure

	githubActionsAppID = store.GithubActionsAppID
)

var (
	ParseWorkflow          = store.ParseWorkflow
	ParseActionRef         = store.ParseActionRef
	envScopeKey            = store.EnvScopeKey
	exprToString           = store.ExprToString
	jobMessageScopeAndRepo = store.JobMessageScopeAndRepo
	normalizeYAMLValue     = store.NormalizeYAMLValue
	stableWorkflowFileID   = store.StableWorkflowFileID
)
