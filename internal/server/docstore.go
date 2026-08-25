package server

import (
	"context"
	"fmt"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/docstore"
	"github.com/notshekhar/drover/internal/object"
)

// registerStore makes a document store reachable through the file jail.
//
// A store is a named prefix rather than a subdirectory, because spec.path can
// point it anywhere. Registering it is what puts it in `ls`, and the writable
// flag is what the one write tool checks -- the jail decides what may be
// written, not the tool.
func (s *Server) registerStore(o *object.Object) {
	spec, err := o.DocumentStore()
	if err != nil {
		s.logf("documentstore %s: %v", o.Metadata.Name, err)
		return
	}
	name := o.Metadata.Name
	if err := s.docs.Ensure(context.Background(), name, spec); err != nil {
		s.logf("documentstore %s: %v", name, err)
		_ = s.store.MarkFailed(object.KindDocumentStore, name, err)
		return
	}
	s.files.SetRoot(docstore.PathPrefix(name), s.docs.Dir(name, spec), spec.IsWritable())
	_ = s.store.MarkReady(object.KindDocumentStore, name, "", "")
}

// registerStores is the bootstrap pass, so a restart finds every store where
// it left it.
func (s *Server) registerStores() {
	objs, err := s.store.List(object.KindDocumentStore)
	if err != nil {
		return
	}
	for _, o := range objs {
		s.registerStore(o)
	}
}

// storeFor resolves the store a path belongs to, and its spec.
func (s *Server) storeFor(name string) (*object.DocumentStoreSpec, error) {
	o, err := s.store.Get(object.KindDocumentStore, name)
	if err != nil {
		return nil, fmt.Errorf("no document store called %q; `drover get documentstore` lists them", name)
	}
	return o.DocumentStore()
}

// writeDocument is the one write path in drover.
func (s *Server) writeDocument(ctx context.Context, store string, req api.DocWriteRequest, author string) (*api.DocWriteResponse, error) {
	spec, err := s.storeFor(store)
	if err != nil {
		return nil, err
	}
	if err := docstore.ValidateDocumentPath(req.Path); err != nil {
		return nil, err
	}

	// Containment is the jail's job, not this function's: one jail, checked
	// in one place, for reads and writes alike.
	full := docstore.PathPrefix(store) + "/" + req.Path
	abs, err := s.files.Writes(full)
	if err != nil {
		return nil, err
	}

	res, err := s.docs.Write(ctx, abs, docstore.WriteRequest{
		Store:   store,
		Rel:     req.Path,
		Content: req.Content,
		Reason:  req.Reason,
		Author:  author,
	}, spec)
	if err != nil {
		return nil, err
	}
	return &api.DocWriteResponse{
		Path:      docstore.PathPrefix(store) + "/" + res.Path,
		Bytes:     res.Bytes,
		Created:   res.Created,
		Commit:    res.Commit,
		Unchanged: res.Unchanged,
	}, nil
}

// documentStoreViews is the catalogue the write tool's description carries.
func (s *Server) documentStoreViews() []api.DocumentStoreView {
	objs, err := s.store.List(object.KindDocumentStore)
	if err != nil {
		return nil
	}
	var out []api.DocumentStoreView
	for _, o := range objs {
		spec, err := o.DocumentStore()
		if err != nil {
			continue
		}
		out = append(out, api.DocumentStoreView{
			Name:        o.Metadata.Name,
			Description: spec.Description,
			Path:        docstore.PathPrefix(o.Metadata.Name),
			Writable:    spec.IsWritable(),
			Documents:   s.docs.Count(o.Metadata.Name, spec),
		})
	}
	return out
}
