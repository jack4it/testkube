package testkube

import (
	"fmt"
	"time"

	"github.com/kubeshop/testkube/pkg/rand"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func NewQueuedTestSuiteExecution(name, namespace string) TestSuiteExecution {
	return TestSuiteExecution{
		TestSuite: &ObjectRef{
			Name:      name,
			Namespace: namespace,
		},
		Status: TestSuiteExecutionStatusQueued,
	}
}

func NewStartedTestSuiteExecution(testSuite TestSuite, request TestSuiteExecutionRequest) TestSuiteExecution {
	testExecution := TestSuiteExecution{
		Id:        primitive.NewObjectID().Hex(),
		StartTime: time.Now(),
		Name:      fmt.Sprintf("%s.%s", testSuite.Name, rand.Name()),
		Status:    TestSuiteExecutionStatusRunning,
		Variables: testSuite.Variables,
		TestSuite: testSuite.GetObjectRef(),
		Labels:    testSuite.Labels,
	}

	// override variables from request
	for k, v := range request.Variables {
		testExecution.Variables[k] = v
	}

	// add queued execution steps
	steps := append(testSuite.Before, testSuite.Steps...)
	steps = append(steps, testSuite.After...)

	for i := range steps {
		testExecution.StepResults = append(testExecution.StepResults, NewTestStepQueuedResult(&steps[i]))
	}

	return testExecution
}

func (e TestSuiteExecution) IsCompleted() bool {
	return *e.Status == *TestSuiteExecutionStatusFailed || *e.Status == *TestSuiteExecutionStatusPassed
}

func (e *TestSuiteExecution) CalculateDuration() time.Duration {

	end := e.EndTime
	start := e.StartTime

	if start.UnixNano() <= 0 && end.UnixNano() <= 0 {
		return time.Duration(0)
	}

	if end.UnixNano() <= 0 {
		end = time.Now()
	}

	return end.Sub(e.StartTime)
}

func (e TestSuiteExecution) Table() (header []string, output [][]string) {
	header = []string{"Status", "Step", "ID", "Error"}
	output = make([][]string, 0)

	for _, sr := range e.StepResults {
		status := "no-execution-result"
		if sr.Execution != nil && sr.Execution.ExecutionResult != nil && sr.Execution.ExecutionResult.Status != nil {
			status = string(*sr.Execution.ExecutionResult.Status)
		}

		switch sr.Step.Type() {
		case TestSuiteStepTypeExecuteTest:
			var id, errorMessage string
			if sr.Execution != nil && sr.Execution.ExecutionResult != nil {
				errorMessage = sr.Execution.ExecutionResult.ErrorMessage
				id = sr.Execution.Id
			}
			row := []string{status, sr.Step.FullName(), id, errorMessage}
			output = append(output, row)
		case TestSuiteStepTypeDelay:
			row := []string{status, sr.Step.FullName(), "", ""}
			output = append(output, row)
		}
	}

	return
}

func (e *TestSuiteExecution) IsRunning() bool {
	return *e.Status == RUNNING_TestSuiteExecutionStatus
}

func (e *TestSuiteExecution) IsQueued() bool {
	return *e.Status == QUEUED_TestSuiteExecutionStatus
}

func (e *TestSuiteExecution) IsPassed() bool {
	return *e.Status == PASSED_TestSuiteExecutionStatus
}

func (e *TestSuiteExecution) IsFailed() bool {
	return *e.Status == FAILED_TestSuiteExecutionStatus
}
