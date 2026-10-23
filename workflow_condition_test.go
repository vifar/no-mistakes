package main

import (
	"fmt"
	"strings"
	"testing"
)

type workflowConditionContext struct {
	PRNumber   string
	RunID      string
	EventName  string
	Ref        string
	Repository string
	HeadRepo   string
	Author     string
	Cancelled  bool
	Failed     bool
	Skipped    bool
}

func evaluateWorkflowCondition(expression string, context workflowConditionContext) (bool, error) {
	evaluator := workflowConditionEvaluator{context: context}
	return evaluator.evaluate(expression)
}

type workflowConditionEvaluator struct {
	context        workflowConditionContext
	hasStatusCheck bool
}

func evaluateWorkflowJobCondition(expression string, context workflowConditionContext) (bool, error) {
	evaluator := workflowConditionEvaluator{context: context}
	value, err := evaluator.evaluate(expression)
	return value && (evaluator.hasStatusCheck || (!context.Cancelled && !context.Failed && !context.Skipped)), err
}

func (e *workflowConditionEvaluator) evaluate(expression string) (bool, error) {
	expression = strings.TrimSpace(expression)
	if strings.HasPrefix(expression, "${{") && strings.HasSuffix(expression, "}}") {
		expression = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expression, "${{"), "}}"))
	}
	return e.boolean(expression)
}

func (e *workflowConditionEvaluator) boolean(expression string) (bool, error) {
	expression, err := trimWorkflowParentheses(strings.TrimSpace(expression))
	if err != nil {
		return false, err
	}
	if expression == "" {
		return false, fmt.Errorf("empty workflow condition")
	}

	for _, operator := range []string{"||", "&&"} {
		parts, found, err := splitWorkflowCondition(expression, operator)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		result := operator == "&&"
		for _, part := range parts {
			value, err := e.boolean(part)
			if err != nil {
				return false, err
			}
			if operator == "||" {
				result = result || value
			} else {
				result = result && value
			}
		}
		return result, nil
	}

	if strings.HasPrefix(expression, "!") {
		value, err := e.boolean(strings.TrimSpace(strings.TrimPrefix(expression, "!")))
		return !value, err
	}
	switch expression {
	case "cancelled()":
		e.hasStatusCheck = true
		return e.context.Cancelled, nil
	case "success()":
		e.hasStatusCheck = true
		return !e.context.Cancelled && !e.context.Failed && !e.context.Skipped, nil
	case "failure()":
		e.hasStatusCheck = true
		return e.context.Failed, nil
	case "always()":
		e.hasStatusCheck = true
		return true, nil
	}

	for _, operator := range []string{"==", "!="} {
		parts, found, err := splitWorkflowCondition(expression, operator)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		if len(parts) != 2 {
			return false, fmt.Errorf("workflow comparison %q has %d operands", expression, len(parts))
		}
		left, err := workflowConditionValue(parts[0], e.context)
		if err != nil {
			return false, err
		}
		right, err := workflowConditionValue(parts[1], e.context)
		if err != nil {
			return false, err
		}
		if operator == "==" {
			return left == right, nil
		}
		return left != right, nil
	}

	return false, fmt.Errorf("unsupported workflow condition %q", expression)
}

func TestWorkflowJobConditionStatusChecks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		expression string
		context    workflowConditionContext
		want       bool
	}{
		{"implicit success", "github.event_name == 'push'", workflowConditionContext{EventName: "push"}, true},
		{"implicit failure", "github.event_name == 'push'", workflowConditionContext{EventName: "push", Failed: true}, false},
		{"implicit skipped", "github.event_name == 'push'", workflowConditionContext{EventName: "push", Skipped: true}, false},
		{"implicit cancellation", "github.event_name == 'push'", workflowConditionContext{EventName: "push", Cancelled: true}, false},
		{"explicit success suppresses failed dependency", "success() && !cancelled() && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository)", workflowConditionContext{EventName: "push", Failed: true}, false},
		{"explicit success allows successful dependency", "success() && !cancelled() && (github.event_name == 'push')", workflowConditionContext{EventName: "push"}, true},
		{"status check overrides implicit success", "${{ !cancelled() && github.event_name == 'push' }}", workflowConditionContext{EventName: "push", Failed: true}, true},
		{"cancellation suppresses explicit condition", "!cancelled() && github.event_name == 'push'", workflowConditionContext{EventName: "push", Cancelled: true}, false},
		{"failure status", "failure()", workflowConditionContext{Failed: true}, true},
		{"failure status after success", "failure()", workflowConditionContext{}, false},
		{"always includes cancellation", "always()", workflowConditionContext{Cancelled: true}, true},
		{"quoted function is not a status check", "github.event_name != 'cancelled()'", workflowConditionContext{EventName: "push", Failed: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evaluateWorkflowJobCondition(tc.expression, tc.context)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("job admission = %t, want %t", got, tc.want)
			}
		})
	}
}

func workflowConditionValue(expression string, context workflowConditionContext) (string, error) {
	expression = strings.TrimSpace(expression)
	if len(expression) >= 2 && expression[0] == '\'' && expression[len(expression)-1] == '\'' {
		return strings.ReplaceAll(expression[1:len(expression)-1], "''", "'"), nil
	}
	switch expression {
	case "github.event_name":
		return context.EventName, nil
	case "github.event.pull_request.number":
		return context.PRNumber, nil
	case "github.run_id":
		return context.RunID, nil
	case "github.ref":
		return context.Ref, nil
	case "github.repository":
		return context.Repository, nil
	case "github.event.pull_request.head.repo.full_name":
		return context.HeadRepo, nil
	case "github.event.pull_request.user.login":
		return context.Author, nil
	}
	return "", fmt.Errorf("unsupported workflow condition value %q", expression)
}

func trimWorkflowParentheses(expression string) (string, error) {
	for strings.HasPrefix(expression, "(") {
		depth := 0
		quoted := false
		closesAtEnd := false
		for i := 0; i < len(expression); i++ {
			switch expression[i] {
			case '\'':
				quoted = !quoted
			case '(':
				if !quoted {
					depth++
				}
			case ')':
				if !quoted {
					depth--
					if depth < 0 {
						return "", fmt.Errorf("unbalanced workflow condition %q", expression)
					}
					if depth == 0 {
						closesAtEnd = i == len(expression)-1
						if !closesAtEnd {
							return expression, nil
						}
					}
				}
			}
		}
		if quoted || depth != 0 {
			return "", fmt.Errorf("unbalanced workflow condition %q", expression)
		}
		if !closesAtEnd {
			return expression, nil
		}
		expression = strings.TrimSpace(expression[1 : len(expression)-1])
	}
	return expression, nil
}

func splitWorkflowCondition(expression, operator string) ([]string, bool, error) {
	var parts []string
	start := 0
	depth := 0
	quoted := false
	for i := 0; i < len(expression); i++ {
		switch expression[i] {
		case '\'':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
				if depth < 0 {
					return nil, false, fmt.Errorf("unbalanced workflow condition %q", expression)
				}
			}
		default:
			if !quoted && depth == 0 && strings.HasPrefix(expression[i:], operator) {
				parts = append(parts, strings.TrimSpace(expression[start:i]))
				i += len(operator) - 1
				start = i + 1
			}
		}
	}
	if quoted || depth != 0 {
		return nil, false, fmt.Errorf("unbalanced workflow condition %q", expression)
	}
	if len(parts) == 0 {
		return nil, false, nil
	}
	parts = append(parts, strings.TrimSpace(expression[start:]))
	for _, part := range parts {
		if part == "" {
			return nil, false, fmt.Errorf("empty operand in workflow condition %q", expression)
		}
	}
	return parts, true, nil
}

func workflowAdmissionError(expression string) error {
	for _, tc := range []struct {
		name  string
		event string
		head  string
		ref   string
		want  bool
	}{
		{"same-repository PR", "pull_request", "owner/repo", "refs/heads/main", true},
		{"fork PR on main", "pull_request", "stranger/repo", "refs/heads/main", false},
		{"fork PR merge ref", "pull_request", "stranger/repo", "refs/pull/1/merge", false},
		{"push", "push", "", "refs/heads/main", true},
		{"manual dispatch", "workflow_dispatch", "", "refs/heads/main", true},
	} {
		got, err := evaluateWorkflowCondition(expression, workflowConditionContext{
			EventName: tc.event, Repository: "owner/repo", HeadRepo: tc.head, Ref: tc.ref, Author: "human-contributor",
		})
		if err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		}
		if got != tc.want {
			return fmt.Errorf("%s runs = %t, want %t", tc.name, got, tc.want)
		}
	}
	return nil
}
