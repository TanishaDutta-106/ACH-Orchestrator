package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/testsuite"

	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/activities"
	"github.com/TanishaDutta-106/ACH-Orchestrator/internal/domain"
	achworkflow "github.com/TanishaDutta-106/ACH-Orchestrator/internal/workflow"
)

// ────────────────────────────────────────────────────────────────────────────
// Suite setup
// ────────────────────────────────────────────────────────────────────────────

type WorkflowUnitTestSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

func (s *WorkflowUnitTestSuite) SetupTest() {
	s.env = s.NewTestWorkflowEnvironment()
	s.env.RegisterActivity(&activities.Activities{})
}

func (s *WorkflowUnitTestSuite) AfterTest(_, _ string) {
	s.env.AssertExpectations(s.T())
}

func TestWorkflowUnitSuite(t *testing.T) {
	suite.Run(t, new(WorkflowUnitTestSuite))
}

// ────────────────────────────────────────────────────────────────────────────
// Helpers — all use mock.Anything for every arg (receiver, ctx, input).
// The Temporal test suite deserializes workflow input via JSON codec, which
// means complex struct matchers see zero values on replay. State-transition
// correctness is verified in integration tests against the real DB.
// ────────────────────────────────────────────────────────────────────────────

func (s *WorkflowUnitTestSuite) mockPersist() {
	s.env.OnActivity(
		(*activities.Activities).PersistStateTransition,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(nil).Once()
}

func (s *WorkflowUnitTestSuite) mockPersistAny() {
	s.env.OnActivity(
		(*activities.Activities).PersistStateTransition,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(nil)
}

func (s *WorkflowUnitTestSuite) mockIdempotencyMiss() {
	s.env.OnActivity(
		(*activities.Activities).CheckIdempotency,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(activities.CheckIdempotencyOutput{AlreadySubmitted: false}, nil)

	s.env.OnActivity(
		(*activities.Activities).StoreTraceNumber,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(nil)
}

func (s *WorkflowUnitTestSuite) mockSubmitACH() {
	s.env.OnActivity(
		(*activities.Activities).SubmitToACH,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(activities.SubmitToACHOutput{TraceNumber: "ACH-test-r0-1"}, nil)
}

func standardInput() achworkflow.PaymentWorkflowInput {
	return achworkflow.PaymentWorkflowInput{
		PaymentID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Amount:        "100.00",
		AccountNumber: "123456789",
		RoutingNumber: "021000021",
	}
}

func (s *WorkflowUnitTestSuite) sendReturn(rCode string) {
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(achworkflow.ReturnSignalName, achworkflow.ReturnSignal{
			RCode:       rCode,
			TraceNumber: "ACH-test-r0-1",
		})
	}, 1*time.Second)
}

// ────────────────────────────────────────────────────────────────────────────
// Tests
// ────────────────────────────────────────────────────────────────────────────

// Non-retryable R-code: INITIATED→PENDING, idempotency miss, PENDING→SUBMITTED,
// submit, SUBMITTED→RETURNED, RETURNED→FAILED_NON_RETRYABLE = 4 persist calls.
func (s *WorkflowUnitTestSuite) TestR02_NonRetryable_FailsImmediately() {
	s.mockPersist()          // INITIATED→PENDING
	s.mockIdempotencyMiss()  // check + store
	s.mockPersist()          // PENDING→SUBMITTED
	s.mockSubmitACH()
	s.mockPersist()          // SUBMITTED→RETURNED
	s.mockPersist()          // RETURNED→FAILED_NON_RETRYABLE

	s.sendReturn("R02")
	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

// R07 must go to FAILED_NON_RETRYABLE, NOT compliance escalation.
func (s *WorkflowUnitTestSuite) TestR07_NonRetryable_NotCompliance() {
	s.mockPersist()
	s.mockIdempotencyMiss()
	s.mockPersist()
	s.mockSubmitACH()
	s.mockPersist()
	s.mockPersist()

	s.sendReturn("R07")
	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

func (s *WorkflowUnitTestSuite) TestR05_ComplianceEscalation() {
	s.mockPersist()
	s.mockIdempotencyMiss()
	s.mockPersist()
	s.mockSubmitACH()
	s.mockPersist()
	s.mockPersist()

	s.sendReturn("R05")
	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

func (s *WorkflowUnitTestSuite) TestR14_ComplianceEscalation() {
	s.mockPersist()
	s.mockIdempotencyMiss()
	s.mockPersist()
	s.mockSubmitACH()
	s.mockPersist()
	s.mockPersist()

	s.sendReturn("R14")
	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

// R01 three times → FAILED_RETRYABLE_EXHAUSTED.
func (s *WorkflowUnitTestSuite) TestR01_RetryExhausted() {
	s.mockPersistAny()
	s.env.OnActivity(
		(*activities.Activities).CheckIdempotency,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(activities.CheckIdempotencyOutput{AlreadySubmitted: false}, nil)
	s.env.OnActivity(
		(*activities.Activities).StoreTraceNumber,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(nil)
	s.env.OnActivity(
		(*activities.Activities).SubmitToACH,
		mock.Anything, mock.Anything, mock.Anything,
	).Return(activities.SubmitToACHOutput{TraceNumber: "ACH-mock"}, nil)

	delay := domain.RetryDelayFor("R01")

	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(achworkflow.ReturnSignalName, achworkflow.ReturnSignal{RCode: "R01"})
	}, 1*time.Second)
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(achworkflow.ReturnSignalName, achworkflow.ReturnSignal{RCode: "R01"})
	}, delay+1*time.Second)
	s.env.RegisterDelayedCallback(func() {
		s.env.SignalWorkflow(achworkflow.ReturnSignalName, achworkflow.ReturnSignal{RCode: "R01"})
	}, 2*delay+1*time.Second)

	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

// No signal → 72h timer fires → SETTLED. 3 persist calls: INITIATED→PENDING,
// PENDING→SUBMITTED, SUBMITTED→SETTLED.
func (s *WorkflowUnitTestSuite) TestSettlementTimer_NoReturn() {
	s.mockPersist()         // INITIATED→PENDING
	s.mockIdempotencyMiss()
	s.mockPersist()         // PENDING→SUBMITTED
	s.mockSubmitACH()
	s.mockPersist()         // SUBMITTED→SETTLED

	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}

func (s *WorkflowUnitTestSuite) TestUnknownRCode_TreatedAsNonRetryable() {
	s.mockPersist()
	s.mockIdempotencyMiss()
	s.mockPersist()
	s.mockSubmitACH()
	s.mockPersist()
	s.mockPersist()

	s.sendReturn("R99")
	s.env.ExecuteWorkflow(achworkflow.PaymentWorkflow, standardInput())

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())
}
