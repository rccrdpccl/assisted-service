// Code generated by go-swagger; DO NOT EDIT.

package events

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the generate command

import (
	"net/http"

	"github.com/go-openapi/runtime/middleware"
)

// ListEventsHandlerFunc turns a function with the right signature into a list events handler
type ListEventsHandlerFunc func(ListEventsParams, interface{}) middleware.Responder

// Handle executing the request and returning a response
func (fn ListEventsHandlerFunc) Handle(params ListEventsParams, principal interface{}) middleware.Responder {
	return fn(params, principal)
}

// ListEventsHandler interface for that can handle valid list events params
type ListEventsHandler interface {
	Handle(ListEventsParams, interface{}) middleware.Responder
}

// NewListEvents creates a new http.Handler for the list events operation
func NewListEvents(ctx *middleware.Context, handler ListEventsHandler) *ListEvents {
	return &ListEvents{Context: ctx, Handler: handler}
}

/*ListEvents swagger:route GET /v1/clusters/{cluster_id}/events events listEvents

Lists events for a cluster.

*/
type ListEvents struct {
	Context *middleware.Context
	Handler ListEventsHandler
}

func (o *ListEvents) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	route, rCtx, _ := o.Context.RouteInfo(r)
	if rCtx != nil {
		r = rCtx
	}
	var Params = NewListEventsParams()

	uprinc, aCtx, err := o.Context.Authorize(r, route)
	if err != nil {
		o.Context.Respond(rw, r, route.Produces, route, err)
		return
	}
	if aCtx != nil {
		r = aCtx
	}
	var principal interface{}
	if uprinc != nil {
		principal = uprinc
	}

	if err := o.Context.BindValidRequest(r, route, &Params); err != nil { // bind params
		o.Context.Respond(rw, r, route.Produces, route, err)
		return
	}

	res := o.Handler.Handle(Params, principal) // actually handle the request

	o.Context.Respond(rw, r, route.Produces, route, res)

}