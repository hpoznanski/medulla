package es

import (
	"fmt"

	"github.com/hpoznanski/medulla/internal/config"
)

// Registry holds one Client per configured cluster, in config order.
type Registry struct {
	names   []string
	clients map[string]*Client
}

func NewRegistry(clusters []config.Cluster) (*Registry, error) {
	r := &Registry{clients: map[string]*Client{}}
	for _, cl := range clusters {
		client, err := NewClient(cl)
		if err != nil {
			return nil, err
		}
		r.names = append(r.names, cl.Name)
		r.clients[cl.Name] = client
	}
	return r, nil
}

func (r *Registry) Get(name string) (*Client, error) {
	c, ok := r.clients[name]
	if !ok {
		return nil, fmt.Errorf("unknown cluster %q", name)
	}
	return c, nil
}

func (r *Registry) Names() []string { return r.names }
