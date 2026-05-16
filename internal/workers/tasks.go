package workers

// TypeProcessPayroll is the Asynq task type identifier for payroll processing jobs.
// It is used both when enqueuing (in PayrollService) and when registering the
// handler (in startWorker) to ensure the correct handler is dispatched.
const (
	TypeProcessPayroll = "payroll:process"
)
