package composeexec

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrInvalidServiceModel = errors.New("invalid effective Compose Service model")

// ServiceDecision is the bounded v1 mutation policy for one effective Service.
// It is suitable for both Agent admission and UI availability; neither caller
// needs to interpret Compose YAML or CLI error text.
type ServiceDecision struct {
	Service
	PullAvailable     bool
	UpAvailable       bool
	UnavailableReason string
}

type V1Policy struct {
	Services           []ServiceDecision
	PullServices       []string
	ProjectUpAvailable bool
	ProjectUpReason    string
	byName             map[string]ServiceDecision
}

func EvaluateV1Policy(models []Service) (V1Policy, error) {
	byModel := make(map[string]Service, len(models))
	for _, model := range models {
		if model.Name == "" {
			return V1Policy{}, fmt.Errorf("%w: empty Service name", ErrInvalidServiceModel)
		}
		if _, duplicate := byModel[model.Name]; duplicate {
			return V1Policy{}, fmt.Errorf("%w: duplicate Service %q", ErrInvalidServiceModel, model.Name)
		}
		byModel[model.Name] = model
	}
	if len(byModel) == 0 {
		return V1Policy{}, fmt.Errorf("%w: no Services", ErrInvalidServiceModel)
	}
	for _, model := range models {
		for _, dependency := range model.DependsOn {
			if _, exists := byModel[dependency]; !exists {
				return V1Policy{}, fmt.Errorf("%w: Service %q dependency %q is absent", ErrInvalidServiceModel, model.Name, dependency)
			}
		}
	}

	names := make([]string, 0, len(byModel))
	for name := range byModel {
		names = append(names, name)
	}
	sort.Strings(names)
	policy := V1Policy{ProjectUpAvailable: true, byName: make(map[string]ServiceDecision, len(names))}
	projectBlocked := make([]string, 0)
	for _, name := range names {
		model := byModel[name]
		decision := ServiceDecision{Service: cloneService(model)}
		switch {
		case !model.Active:
			decision.UnavailableReason = "Service is excluded by an inactive Compose profile"
		case model.Image == "":
			decision.UnavailableReason = "DockLattice v1 does not build Images; this Service has no declared Image"
		case model.HasBuild && model.PullPolicy == "build":
			decision.UnavailableReason = "Compose pull_policy requires an Image build, which DockLattice v1 does not perform"
		default:
			decision.PullAvailable = true
			blocked, err := buildRequiredClosure(name, byModel)
			if err != nil {
				return V1Policy{}, err
			}
			if len(blocked) == 0 {
				decision.UpAvailable = true
			} else {
				decision.UnavailableReason = "Up requires build-required dependency: " + strings.Join(blocked, ", ")
			}
		}
		if decision.PullAvailable {
			policy.PullServices = append(policy.PullServices, name)
		}
		if model.Active && model.BuildRequired() {
			projectBlocked = append(projectBlocked, name)
		}
		policy.Services = append(policy.Services, decision)
		policy.byName[name] = decision
	}
	if len(projectBlocked) != 0 {
		policy.ProjectUpAvailable = false
		policy.ProjectUpReason = "DockLattice v1 does not build Images; build-required Services: " + strings.Join(projectBlocked, ", ")
	}
	return policy, nil
}

func (policy V1Policy) Targets(operation Operation, target string) ([]string, error) {
	switch operation {
	case OperationPull:
		if target == "" {
			if len(policy.PullServices) == 0 {
				return nil, errors.New("the active effective model has no image-backed Service")
			}
			return append([]string(nil), policy.PullServices...), nil
		}
		decision, ok := policy.byName[target]
		if !ok {
			return nil, fmt.Errorf("unknown Service %q", target)
		}
		if !decision.PullAvailable {
			return nil, errors.New(decision.UnavailableReason)
		}
		return []string{target}, nil
	case OperationUp:
		if target == "" {
			if !policy.ProjectUpAvailable {
				return nil, errors.New(policy.ProjectUpReason)
			}
			return nil, nil
		}
		decision, ok := policy.byName[target]
		if !ok {
			return nil, fmt.Errorf("unknown Service %q", target)
		}
		if !decision.UpAvailable {
			return nil, errors.New(decision.UnavailableReason)
		}
		return []string{target}, nil
	default:
		if target == "" {
			return nil, nil
		}
		if _, ok := policy.byName[target]; !ok {
			return nil, fmt.Errorf("unknown Service %q", target)
		}
		return []string{target}, nil
	}
}

func buildRequiredClosure(target string, models map[string]Service) ([]string, error) {
	seen := make(map[string]struct{})
	pending := []string{target}
	blocked := make([]string, 0)
	for len(pending) != 0 {
		name := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		service, ok := models[name]
		if !ok {
			return nil, fmt.Errorf("%w: dependency %q is absent", ErrInvalidServiceModel, name)
		}
		if service.BuildRequired() {
			blocked = append(blocked, name)
		}
		pending = append(pending, service.DependsOn...)
	}
	sort.Strings(blocked)
	return blocked, nil
}

func cloneService(service Service) Service {
	service.Profiles = append([]string(nil), service.Profiles...)
	service.DependsOn = append([]string(nil), service.DependsOn...)
	return service
}
