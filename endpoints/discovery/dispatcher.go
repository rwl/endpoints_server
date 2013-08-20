// Copyright 2007 Google Inc.

package discovery

import (
	"net/http"
	"fmt"
	"log"
	"encoding/json"
	"regexp"
	"errors"
	"io/ioutil"
	"bytes"
	"strings"
)

// Pattern for paths handled by this module.
const API_SERVING_PATTERN = "_ah/api/.*"

const _SPI_ROOT_FORMAT = "/_ah/spi/%s"
const _SERVER_SOURCE_IP = "0.2.0.3"

const _API_EXPLORER_URL = "https://developers.google.com/apis-explorer/?base="

// Dispatcher that handles requests to the built-in apiserver handlers.
type EndpointsDispatcher struct {
	dispatcher Dispatcher // A Dispatcher instance that can be used to make HTTP requests.
	config_manager *ApiConfigManager // An ApiConfigManager instance that allows a caller to set up an existing configuration for testing.
	discovery_api *DiscoveryApiProxy
	dispatchers []dispatcher
}

type Dispatcher interface {
	Do(*http.Request) (*http.Response, error)
}

type dispatcher struct {
	path_regex *regexp.Regexp
	dispatch_func func(http.ResponseWriter, *http.Request)
}

func NewEndpointsDispatcher(dispatcher Dispatcher) *EndpointsDispatcher {
	return NewEndpointsDispatcherConfig(dispatcher, NewApiConfigManager(), NewDiscoveryApiProxy())
}

func NewEndpointsDispatcherConfig(dispatcher Dispatcher,
		config_manager *ApiConfigManager,
		discovery_api *DiscoveryApiProxy) *EndpointsDispatcher {
	d := &EndpointsDispatcher{
		dispatcher,
		config_manager,
		discovery_api,
		make([]dispatcher, 0),
	}
	d.add_dispatcher("/_ah/api/explorer/?$", d.handle_api_explorer_request)
	d.add_dispatcher("/_ah/api/static/.*$", d.handle_api_static_request)
	return d
}

// Add a request path and dispatch handler.

// Args:
// path_regex: A string regex, the path to match against incoming requests.
// dispatch_function: The function to call for these requests.  The function
// should take (request, start_response) as arguments and
// return the contents of the response body.
func (ed *EndpointsDispatcher) add_dispatcher(path_regex string, dispatch_function interface{}) {
	regex, _ := regexp.Compile(path_regex)
	ed.dispatchers = append(ed.dispatchers,
		&dispatcher{regex, dispatch_function})
}

func (ed *EndpointsDispatcher) Handle(w http.ResponseWriter, r *http.Request) {
	ed.dispatch(w, r)
}

func (ed *EndpointsDispatcher) dispatch(w http.ResponseWriter, r *http.Request) string {
	// Check if this matches any of our special handlers.
	dispatched_response, err := ed.dispatch_non_api_requests(w, r)
	if err == nil {
		return dispatched_response
	}

	// Get API configuration first.  We need this so we know how to
	// call the back end.
	api_config_response := ed.get_api_configs()
	if !ed.handle_get_api_configs_response(api_config_response) {
		return ed.fail_request(w, r, "BackendService.getApiConfigs Error")
	}

	// Call the service.
	body, err := ed.call_spi(w, r)
	if err != nil {
		return ed.handle_request_error(r, err, w)
	}
	return body
}

// Dispatch this request if this is a request to a reserved URL.
//
// If the request matches one of our reserved URLs, this calls
// start_response and returns the response body.
//
// Args:
// request: An ApiRequest, the request from the user.
// start_response:
//
// Returns:
// None if the request doesn't match one of the reserved URLs this
// handles.  Otherwise, returns the response body.
func (ed *EndpointsDispatcher) dispatch_non_api_requests(w http.ResponseWriter, r *http.Request) (string, error) {
	for _, d := range ed.dispatchers {
		if d.path_regex.Match([]byte(r.URL.RequestURI)) {
			return ed.dispatch_func(w, r), nil
		}
	}
	return "", errors.New("Doesn't match one of the reserved URL")
}

// Handler for requests to _ah/api/explorer.
//
// This calls start_response and returns the response body.
//
// Args:
// request: An ApiRequest, the request from the user.
// start_response:
//
// Returns:
// A string containing the response body (which is empty, in this case).
func (ed *EndpointsDispatcher) handle_api_explorer_request(w http.ResponseWriter, request *http.Request) string {
	base_url := fmt.Sprintf("http://%s/_ah/api", request.URL.Host)
	redirect_url := _API_EXPLORER_URL + base_url
	return send_redirect_response(redirect_url, w, request, nil)
}

// Handler for requests to _ah/api/static/.*.
//
// This calls start_response and returns the response body.
//
// Args:
// request: An ApiRequest, the request from the user.
// start_response:
//
// Returns:
// A string containing the response body.
func (ed *EndpointsDispatcher) handle_api_static_request(w http.ResponseWriter, request *http.Request) string {
	response, body, err := ed.discovery_api.get_static_file(request.URL.RequestURI)
//	status_string := fmt.Sprintf("%d %s", response.status, response.reason)
	if err == nil && response.StatusCode == 200 {
		// Some of the headers that come back from the server can't be passed
		// along in our response.  Specifically, the response from the server has
		// transfer-encoding: chunked, which doesn't apply to the response that
		// we're forwarding.  There may be other problematic headers, so we strip
		// off everything but Content-Type.
		w.Header().Add("Content-Type", response.Header.Get("Content-Type"))
		fmt.Fprintf(w, body)
	} else {
		log.Printf("Discovery API proxy failed on %s with %d. Details: %s",
			request.URL.ReqestURI, response.StatusCode, body)
		http.Error(w, body, response.StatusCode)
//		return send_response(status_string, response.getheaders(), body, start_response)
	}
	return body
}

// Makes a call to the BackendService.getApiConfigs endpoint.
//
// Returns:
// A ResponseTuple containing the response information from the HTTP
// request.
func (ed *EndpointsDispatcher) get_api_configs() *http.Response {
	req, err := http.NewRequest("POST",
		_SERVER_SOURCE_IP + "/_ah/spi/BackendService.getApiConfigs",
		ioutil.NopCloser(bytes.NewBufferString("{}")))
	if err != nil {
		return nil // fixme: handle error
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := ed.dispatcher.Do(req)
	if err != nil {
		return nil
	}
	return resp
}

// Verifies that a response has the expected status and content type.
//
// Args:
// response: The ResponseTuple to be checked.
// status_code: An int, the HTTP status code to be compared with response
// status.
// content_type: A string with the acceptable Content-Type header value.
// None allows any content type.
//
// Returns:
// True if both status_code and content_type match, else False.
func verify_response(response *http.Response, status_code int, content_type string) bool {
//	status = int(response.StatusCode.split(" ", 1)[0])
	if response.StatusCode != status_code {
		return false
	}
	if len(content_type) == 0 {
		return true
	}
	ct := response.Header.Get("Content-Type")
	if len(ct) == 0 {
		return false
	}
	if ct == content_type {
		return true
	}
	return false
}

// Parses the result of GetApiConfigs and stores its information.
//
// Args:
//   api_config_response: The http.Response from the GetApiConfigs call.
//
// Returns:
//   True on success, False on failure
func (ed *EndpointsDispatcher) handle_get_api_configs_response(api_config_response *http.Response) bool {
	if ed.verify_response(api_config_response, 200, "application/json") {
		body, err := ioutil.ReadAll(api_config_response.Body)
		defer api_config_response.Body.Close()
		if err != nil {
			return false
		}
		ed.config_manager.parse_api_config_response(string(body))
		return true
	}
	return false
}

// Generate SPI call (from earlier-saved request).
//
// This calls start_response and returns the response body.
//
// Args:
// orig_request: An ApiRequest, the original request from the user.
// start_response:
//
// Returns:
// A string containing the response body.
func (ed *EndpointsDispatcher) call_spi(w http.ResponseWriter, orig_request *ApiRequest) (string, error) {
	var method_config *ApiMethod
	var params map[string]string
	if orig_request.is_rpc() {
		method_config = ed.lookup_rpc_method(orig_request)
		params = nil
	} else {
		method_config, params = ed.lookup_rest_method(orig_request)
	}
	if method_config == nil {
		cors_handler := newCheckCorsHeaders(orig_request)
		return send_not_found_response(w, cors_handler), nil
	}

	// Prepare the request for the back end.
	spi_request := ed.transform_request(orig_request, params, method_config)

	// Check if this SPI call is for the Discovery service. If so, route
	// it to our Discovery handler.
	discovery := NewDiscoveryService(ed.config_manager)
	discovery_response, ok := discovery.handle_discovery_request(spi_request.path,
		spi_request, w)
	if ok {
		return discovery_response, nil
	}

	// Send the request to the user's SPI handlers.
	url := fmt.Sprintf(_SPI_ROOT_FORMAT, spi_request.path)
	req, err := http.NewRequest("POST", url, spi_request.Body)
	if err != nil {
		return "", err
	}
	req.Header.Add("Content-Type", "application/json")
	req.RemoteAddr = spi_request.RemoteAddr
	resp, err := ed.dispatcher.Do(req)
	if err != nil {
		return "", err
	}
	return ed.handle_spi_response(orig_request, spi_request, resp, w)
}

// Handle SPI response, transforming output as needed.
//
// This calls start_response and returns the response body.
//
// Args:
// orig_request: An ApiRequest, the original request from the user.
// spi_request: An ApiRequest, the transformed request that was sent to the
// SPI handler.
// response: A ResponseTuple, the response from the SPI handler.
// start_response:
//
// Returns:
// A string containing the response body.
func (ed *EndpointsDispatcher) handle_spi_response(orig_request, spi_request *ApiRequest, response *http.Response, w http.ResponseWriter) (string, error) {
	resp_body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	response.Body.Close()

	// Verify that the response is json.  If it isn"t treat, the body as an
	// error message and wrap it in a json error response.
	for header, value := range response.Header {
		if header == "Content-Type" && !strings.HasPrefix(value, "application/json") {
			return ed.fail_request(orig_request, fmt.Sprintf("Non-JSON reply: %s", resp_body), w)
		}
	}

	err := ed.check_error_response(response)
	if err != nil {
		return "", err
	}

	// Need to check is_rpc() against the original request, because the
	// incoming request here has had its path modified.
	var body string
	if orig_request.is_rpc() {
		body = ed.transform_jsonrpc_response(spi_request, string(resp_body))
	} else {
		body = ed.transform_rest_response(string(resp_body))
	}

	cors_handler := newCheckCorsHeaders(orig_request)
	cors_handler.UpdateHeaders(w.Header())
	fmt.Fprint(w, body)
//	return send_response(response.status, response.headers, body, w, /*cors_handler=*/cors_handler)
	return body, nil
}

// Write an immediate failure response to outfile, no redirect.
//
// This calls start_response and returns the error body.
//
// Args:
// orig_request: An ApiRequest, the original request from the user.
// message: A string containing the error message to be displayed to user.
// start_response:
//
// Returns:
// A string containing the body of the error response.
func (ed *EndpointsDispatcher) fail_request(w http.ResponseWriter, orig_request *http.Request, message string) string {
	cors_handler := newCheckCorsHeaders(orig_request)
	return send_error_response(message, w, cors_handler)
}

// Looks up and returns rest method for the currently-pending request.
//
// Args:
// orig_request: An ApiRequest, the original request from the user.
//
// Returns:
// A tuple of (method descriptor, parameters), or (None, None) if no method
// was found for the current request.
func (ed *EndpointsDispatcher) lookup_rest_method(orig_request *ApiRequest) (*ApiMethod, map[string]string) {
	method_name, method, params := ed.config_manager.lookup_rest_method(orig_request.path, orig_request.http_method)
	orig_request.method_name = method_name
	return method, params
}

// Looks up and returns RPC method for the currently-pending request.
//
// Args:
// orig_request: An ApiRequest, the original request from the user.
//
// Returns:
// The RPC method descriptor that was found for the current request, or None
// if none was found.
func (ed *EndpointsDispatcher) lookup_rpc_method(orig_request *ApiRequest) *ApiMethod {
	if !orig_request.body_json {
		return nil
	}
	method_name, ok := orig_request.body_json["method"]
	if !ok {
		method_name = ""
	}
	version, ok := orig_request.body_json["apiVersion"]
	if !ok {
		version = ""
	}
	orig_request.Method = method_name
	return ed.config_manager.lookup_rpc_method(method_name, version)
}

// Transforms orig_request to apiserving request.
//
// This method uses orig_request to determine the currently-pending request
// and returns a new transformed request ready to send to the SPI.  This
// method accepts a rest-style or RPC-style request.
//
// Args:
// orig_request: An ApiRequest, the original request from the user.
// params: A dictionary containing path parameters for rest requests, or
// None for an RPC request.
// method_config: A dict, the API config of the method to be called.
//
// Returns:
// An ApiRequest that"s a copy of the current request, modified so it can
// be sent to the SPI.  The path is updated and parts of the body or other
// properties may also be changed.
func (ed *EndpointsDispatcher) transform_request(orig_request *ApiRequest, params map[string]string, method_config *ApiMethod) *ApiRequest {
	var request *ApiRequest
	if orig_request.is_rpc() {
		request = ed.transform_jsonrpc_request(orig_request)
	} else {
		method_params := method_config.Request.Params
		request = ed.transform_rest_request(orig_request, params, method_params)
	}
	request.path = method_config.get("rosyMethod", "")
	return request
}

// Checks if the parameter value is valid if an enum.
//
// If the parameter is not an enum, does nothing. If it is, verifies that
// its value is valid.
//
// Args:
// parameter_name: A string containing the name of the parameter, which is
// either just a variable name or the name with the index appended. For
// example "var" or "var[2]".
// value: A string or list of strings containing the value(s) to be used as
// enum(s) for the parameter.
// field_parameter: The dictionary containing information specific to the
// field in question. This is retrieved from request.parameters in the
// method config.
//
// Raises:
// EnumRejectionError: If the given value is not among the accepted
// enum values in the field parameter.
func (ed *EndpointsDispatcher) check_enum(parameter_name string, value string, field_parameter *ApiRequestParamSpec) *EnumRejectionError {
	if field_parameter.Enum == nil {
		return nil
	}

	enum_values := make([]string, 0)
	for _, enum := range field_parameter.Enum {
		if enum.BackendVal != nil {
			enum_values = append(enum_values, enum.BackendVal)
		}
	}

	for _, ev := range enum_values {
		if value == ev {
			return nil
		}
	}
	return NewEnumRejectionError(parameter_name, value, enum_values)
}

// Recursively calls check_parameter on the values in the list.
//
// "[index-of-value]" is appended to the parameter name for
// error reporting purposes.
func (ed *EndpointsDispatcher) check_parameters(parameter_name, value []string, field_parameter *ApiRequestParamSpec) {
	for index, element := range value {
		parameter_name_index := fmt.Sprintf("%s[%d]", parameter_name, index)
		ed.check_parameter(parameter_name_index, element, field_parameter)
	}
}

// Checks if the parameter value is valid against all parameter rules.
//
// Currently only checks if value adheres to enum rule, but more checks may be
// added.
//
// Args:
//   parameter_name: A string containing the name of the parameter, which is
//     either just a variable name or the name with the index appended, in the
//     recursive case. For example "var" or "var[2]".
//   value: A string containing the value to be used for the parameter.
//   field_parameter: The dictionary containing information specific to the
//     field in question. This is retrieved from request.parameters in the
//     method config.
func (ed *EndpointsDispatcher) check_parameter(parameter_name, value string, field_parameter *ApiRequestParamSpec) {
	ed.check_enum(parameter_name, value, field_parameter)
}

// Converts a . delimitied field name to a message field in parameters.
//
// This adds the field to the params dict, broken out so that message
// parameters appear as sub-dicts within the outer param.
//
// For example:
//   {"a.b.c": ["foo"]}
// becomes:
//   {"a": {"b": {"c": ["foo"]}}}
//
// Args:
//   field_name: A string containing the "." delimitied name to be converted
// into a dictionary.
// value: The value to be set.
// params: The dictionary holding all the parameters, where the value is
// eventually set.
func (ed *EndpointsDispatcher) add_message_field(field_name string, value interface{}, params JsonObject) {
	pos := strings.Index(field_name, ".")
	if pos == -1 {
		params[field_name] = value
		return
	}

	substr := strings.SplitN(field_name, ".", 1)
	root, remaining := substr[0], substr[1]

	var sub_params JsonObject
	_sub_params, ok := params[root]
	if ok {
		sub_params, _ = _sub_params.(JsonObject)
	} else {
		sub_params = make(JsonObject)
	}
	ed.add_message_field(remaining, value, sub_params)
}

// Updates the dictionary for an API payload with the request body.
//
// The values from the body should override those already in the payload, but
// for nested fields (message objects) the values can be combined
// recursively.
//
// Args:
//   destination: A dictionary containing an API payload parsed from the
//     path and query parameters in a request.
//   source: A dictionary parsed from the body of the request.
func (ed *EndpointsDispatcher) update_from_body(destination JsonObject, source JsonObject) {
	for key, value := range source {
		destination_value, ok := destination[key]
		if ok {
			_, ok_val := value.(JsonObject)
			dest_value, ok_dest := destination_value.(JsonObject)
			if ok_val && ok_dest {
				ed.update_from_body(dest_value, value)
			} else {
				destination[key] = value
			}
		} else {
			destination[key] = value
		}
	}
}

// Translates a Rest request into an apiserving request.
//
// This makes a copy of orig_request and transforms it to apiserving
// format (moving request parameters to the body).
//
// The request can receive values from the path, query and body and combine
// them before sending them along to the SPI server. In cases of collision,
// objects from the body take precedence over those from the query, which in
// turn take precedence over those from the path.
//
// In the case that a repeated value occurs in both the query and the path,
// those values can be combined, but if that value also occurred in the body,
// it would override any other values.
//
// In the case of nested values from message fields, non-colliding values
// from subfields can be combined. For example, if "?a.c=10" occurs in the
// query string and "{"a": {"b": 11}}" occurs in the body, then they will be
// combined as
//
// {
//   "a": {
//     "b": 11,
//     "c": 10,
//   }
// }
//
// before being sent to the SPI server.
//
// Args:
//   orig_request: An ApiRequest, the original request from the user.
//   params: A dict with URL path parameters extracted by the config_manager
//     lookup.
//   method_parameters: A dictionary containing the API configuration for the
//     parameters for the request.
//
// Returns:
//   A copy of the current request that's been modified so it can be sent
//   to the SPI.  The body is updated to include parameters from the URL.
func (ed *EndpointsDispatcher) transform_rest_request(orig_request *ApiRequest,
		params map[string]string,
		method_parameters map[string]*ApiRequestParamSpec) *ApiRequest {

	request := orig_request.copy()
	body_json := make(JsonObject)

	// Handle parameters from the URL path.
	for key, value := range params {
		// Values need to be in a list to interact with query parameter values
		// and to account for case of repeated parameters
		body_json[key] = []string{value}
	}

	// Add in parameters from the query string.
	if len(request.URL.RawQuery) > 0 {
		// For repeated elements, query and path work together
		for key, value := range request.URL.Query {
			if _, ok := body_json[key]; ok {
				body_json[key] = value + body_json[key]
			} else {
				body_json[key] = value
			}
		}
	}

	// Validate all parameters we've merged so far and convert any "." delimited
	// parameters to nested parameters.  We don't use iteritems since we may
	// modify body_json within the loop.  For instance, "a.b" is not a valid key
	// and would be replaced with "a".
	for key, value := range body_json {
		current_parameter, ok := method_parameters[key]
		repeated := false
		if ok {
			repeated = current_parameter.Repeated
		}

		if !repeated {
			val := body_json[key]
			val_arr, ok := val.([]interface{})
			if ok {
				body_json[key] = val_arr[0]
			}
		}

		// Order is important here. Parameter names are dot-delimited in
		// parameters instead of nested in maps as a message field is, so
		// we need to call _check_parameter on them before calling
		// _add_message_field.

		ed.check_parameter(key, body_json[key], current_parameter)
		// Remove the old key and try to convert to nested message value
		message_value := body_json[key]
		delete(body_json, key)
		ed.add_message_field(key, message_value, body_json)
	}

	// Add in values from the body of the request.
	if request.body_json != nil {
		ed.update_from_body(body_json, request.body_json)
	}

	request.body_json = body_json
	body, err := json.Marshal(request.body_json)
	if err == nil {
		request.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	}
	return request
}

// Translates a JsonRpc request/response into apiserving request/response.
//
// Args:
//   orig_request: An ApiRequest, the original request from the user.
//
// Returns:
//   A new request with the request_id updated and params moved to the body.
func (ed *EndpointsDispatcher) transform_jsonrpc_request(orig_request *ApiRequest) (*ApiRequest, error) {
	request := orig_request.copy()
	request.request_id, _ = request.body_json["id"]
	request.body_json, _ = request.body_json["params"]
	body, err := json.Marshal(request.body_json)
	if err != nil {
		return request, fmt.Errorf("Problem transforming RPC request: %s", err.Error())
	}
	request.Body = ioutil.NopCloser(bytes.NewBuffer(body))
	return request, nil
}

// Returns an error if the response from the SPI was an error.
//
// Args:
//   response: A http.Response containing the backend response.
//
// Returns:
//   BackendError if the response is an error.
func (ed *EndpointsDispatcher) check_error_response(response *http.Response) error {
	if response.StatusCode >= 300 {
		return NewBackendError(response)
	}
	return nil
}

// Translates an apiserving REST response so it's ready to return.
//
// Currently, the only thing that needs to be fixed here is indentation,
// so it's consistent with what the live app will return.
//
// Args:
//   response_body: A string containing the backend response.
//
// Returns:
//   A reformatted version of the response JSON.
func (ed *EndpointsDispatcher) transform_rest_response(response_body string) (string, error) {
	var body_json JsonObject
	err := json.Unmarshal(response_body, &body_json)
	if err != nil {
		return response_body, fmt.Errorf("Problem transforming REST response: %s", err.Error())
	}
	body, _ := json.MarshalIndent(body_json, "", "  ") // todo: sort keys
	return string(body)
}

// Translates an apiserving response to a JsonRpc response.
//
// Args:
//   spi_request: An ApiRequest, the transformed request that was sent to the
//     SPI handler.
//   response_body: A string containing the backend response to transform
//     back to JsonRPC.
//
// Returns:
//   A string with the updated, JsonRPC-formatted request body.
func (ed *EndpointsDispatcher) transform_jsonrpc_response(spi_request *ApiRequest, response_body string) (string, error) {
	var result interface{}
	err := json.Unmarshal(response_body, &result)
	if err != nil {
		return response_body, fmt.Errorf("Problem unmarshalling RPC response: %s", err.Error())
	}
	body_json := JsonObject{"result": result}
	return ed.finish_rpc_response(spi_request.request_id, spi_request.is_batch(), body_json)
}

// Finish adding information to a JSON RPC response.
//
// Args:
//   request_id: None if the request didn't have a request ID.  Otherwise, this
//     is a string containing the request ID for the request.
//   is_batch: A boolean indicating whether the request is a batch request.
//   body_json: A dict containing the JSON body of the response.
//
// Returns:
//   A string with the updated, JsonRPC-formatted request body.
func (ed *EndpointsDispatcher) finish_rpc_response(request_id string, is_batch bool, body_json JsonObject) string {
	if len(request_id) > 0 {
		body_json["id"] = request_id
	}
	if is_batch {
		body_json = []JsonObject{body_json}
	}
	body, _ := json.MarshalIndent(body_json, "", "  ") // todo: sort keys
	return string(body)
}

// Handle a request error, converting it to a WSGI response.
//
// Args:
//   orig_request: An ApiRequest, the original request from the user.
//   error: A RequestError containing information about the error.
//   start_response:
//
// Returns:
//   A string containing the response body.
func (ed *EndpointsDispatcher) handle_request_error(w http.ResponseWriter, orig_request *http.Request, err *RequestError) string {
	w.Headers().Add("Content-Type", "application/json")
	var status_code int
	var body string
	if orig_request.is_rpc() {
		// JSON RPC errors are returned with status 200 OK and the
		// error details in the body.
		status_code = 200
		body = ed.finish_rpc_response(orig_request.body_json["id"],
			orig_request.is_batch(), err.rpc_error())
	} else {
		status_code = err.status_code()
		body = err.rest_error()
	}

//	response_status = fmt.Sprintf("%d %s", status_code,
//		http.StatusText(status_code)) // fixme: handle unknown status code "Unknown Error"

	newCheckCorsHeaders(orig_request).UpdateHeaders(w.Header())
	http.Error(w, body, status_code)
//	return send_response(response_status, body, w, cors_handler)
	return body
}

/* Utilities */

func send_not_found_response(w http.ResponseWriter, cors_handler/*=None*/ CorsHandler) string {
	if cors_handler != nil {
		cors_handler.UpdateHeaders(w.Header())
	}
	w.Header().Add("Content-Type", "text/plain")
	body := "Not Found"
	http.Error(w, body, http.StatusNotFound)
//	return send_wsgi_response("404", h, , w, /*cors_handler=*/cors_handler)
	return body
}

func send_error_response(message string, w http.ResponseWriter, cors_handler CorsHandler) string {
	body_map := JsonObject{
		"string": JsonObject{
			"message": message,
		},
	}
	body, _ := json.Marshal(body_map)
//	header := make(http.Header)
	if cors_handler != nil {
		cors_handler.UpdateHeaders(w.Header())
	}
	w.Header().Add("Content-Type", "application/json")
	http.Error(w, string(body), http.StatusInternalServerError)
//	return send_response("500", header, string(body), w, /*cors_handler=*/cors_handler)
	return string(body)
}

func send_rejected_response(rejection_error JsonObject, w http.ResponseWriter, cors_handler/*=None*/ CorsHandler) string {
//	body = rejection_error.to_json()
	body, _ := json.Marshal(rejection_error)
	if cors_handler != nil {
		cors_handler.UpdateHeaders(w.Header())
	}
	w.Header().Add("Content-Type", "application/json")
	http.Error(w, string(body), http.StatusBadRequest)
//	return send_response("400", header, body, w, /*cors_handler=*/cors_handler)
	return string(body)
}

func send_redirect_response(redirect_location string, w http.ResponseWriter, r *http.Request, cors_handler/*=None*/ CorsHandler) string {
//	header := make(http.Header)
//	header.Add("Location", redirect_location)
//	return send_response("302", header, "", w, /*cors_handler=*/cors_handler)
	if cors_handler != nil {
		cors_handler.UpdateHeaders(w.Header())
	}
	http.Redirect(w, r, redirect_location, http.StatusFound)
	return ""
}

// Dump reformatted response to CGI start_response.
//
// This calls start_response and returns the response body.
//
// Args:
//   status: A string containing the HTTP status code to send.
//   headers: A list of (header, value) tuples, the headers to send in the
//     response.
//   content: A string containing the body content to write.
//   start_response:
//   cors_handler: A handler to process CORS request headers and update the
//     headers in the response.  Or this can be None, to bypass CORS checks.
//
// Returns:
//   A string containing the response body.
/*func send_response(status int, headers http.Header, content string, w http.ResponseWriter, cors_handler CorsHandler) *http.Response {
	if cors_handler != nil {
		cors_handler.UpdateHeaders(headers)
	}

	// Update content length.
	content_len := len(content)
	headers.append(("Content-Length", fmt.Sprintf("%d", content_len))

	start_response(status, headers)
	return content
}*/
