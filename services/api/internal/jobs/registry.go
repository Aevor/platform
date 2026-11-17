package jobs

import (
	"fmt"
	"sync"
)

type Registry struct {
	mu          sync.RWMutex
	definitions map[string]Definition
}

func NewRegistry() *Registry {
	return &Registry{definitions: make(map[string]Definition)}
}

func (r *Registry) Register(definition Definition) error {
	if definition.Type == "" || len(definition.Type) > 96 || definition.Run == nil {
		return fmt.Errorf("job definition requires a type and run function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.definitions == nil {
		r.definitions = make(map[string]Definition)
	}
	if _, exists := r.definitions[definition.Type]; exists {
		return fmt.Errorf("job definition %q is already registered", definition.Type)
	}
	r.definitions[definition.Type] = definition
	return nil
}

func (r *Registry) MustRegister(definition Definition) {
	if err := r.Register(definition); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(typ string) (Definition, error) {
	if r == nil {
		return Definition{}, ErrUnknownType
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	definition, ok := r.definitions[typ]
	if !ok {
		return Definition{}, ErrUnknownType
	}
	return definition, nil
}
