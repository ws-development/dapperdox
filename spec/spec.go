package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	//"github.com/davecgh/go-spew/spew"
	"github.com/go-openapi/loads"
	"github.com/go-openapi/spec"
	"github.com/serenize/snaker"
	"github.com/shurcooL/github_flavored_markdown"
	"github.com/zxchris/swaggerly/config"
	"github.com/zxchris/swaggerly/logger"
)

type APISpecification struct {
	ID      string
	APIs    APISet // APIs represents the parsed APIs
	APIInfo Info

	SecurityDefinitions map[string]SecurityScheme
	DefaultSecurity     map[string]Security
	ResourceList        map[string]map[string]*Resource // Version->ResourceName->Resource
	APIVersions         map[string]APISet               // Version->APISet
}

var APISuite map[string]*APISpecification

// GetByName returns an API by name
func (c *APISpecification) GetByName(name string) *APIGroup {
	for _, a := range c.APIs {
		if a.Name == name {
			return &a
		}
	}
	return nil
}

// GetByID returns an API by ID
func (c *APISpecification) GetByID(id string) *APIGroup {
	for _, a := range c.APIs {
		if a.ID == id {
			return &a
		}
	}
	return nil
}

type APISet []APIGroup

type Info struct {
	Title       string
	Description string
}

// APIGroup parents all grouped API methods (Grouping controlled by tagging, if used, or by method path otherwise)
type APIGroup struct {
	ID                     string
	Name                   string
	URL                    *url.URL
	MethodNavigationByName bool
	Versions               map[string][]Method // All versions, keyed by version string.
	Methods                []Method            // The current version
	CurrentVersion         string              // The latest version in operation for the API
	Info                   *Info
}

type Version struct {
	Version string
	Methods []Method
}

type OAuth2Scheme struct {
	OAuth2Flow       string
	AuthorizationUrl string
	TokenUrl         string
	Scopes           map[string]string
}

type SecurityScheme struct {
	IsApiKey      bool
	IsBasic       bool
	IsOAuth2      bool
	Type          string
	Description   string
	ParamName     string
	ParamLocation string
	OAuth2Scheme
}

type Security struct {
	Scheme *SecurityScheme
	Scopes map[string]string
}

// Method represents an API method
type Method struct {
	ID              string
	Name            string
	Description     string
	Method          string
	OperationName   string
	NavigationName  string
	Path            string
	PathParams      []Parameter
	QueryParams     []Parameter
	HeaderParams    []Parameter
	BodyParam       *Parameter
	FormParams      []Parameter
	Responses       map[int]Response
	DefaultResponse *Response
	Resources       []*Resource
	Security        map[string]Security
	APIGroup        *APIGroup
}

// Parameter represents an API method parameter
type Parameter struct {
	Name        string
	Description string
	In          string
	Required    bool
	Type        string
	Enum        []string
	Resource    *Resource // For "in body" parameters
}

// Response represents an API method response
type Response struct {
	Description string
	Resource    *Resource // FIXME rename as Resource?
}

// Resource represents an API resource
type Resource struct {
	ID                    string
	FQNS                  []string
	Title                 string
	Description           string
	Example               string
	Schema                string
	Type                  []string
	Properties            map[string]*Resource
	Required              bool
	ReadOnly              bool
	ExcludeFromOperations []string
	Methods               []Method
	Enum                  []string
}

// -----------------------------------------------------------------------------

func LoadSpecifications(host string, collapse bool) error {

	if APISuite == nil {
		APISuite = make(map[string]*APISpecification)
	}

	cfg, err := config.Get()
	if err != nil {
		logger.Errorf(nil, "error configuring app: %s", err)
		return err
	}

	for _, specFilename := range cfg.SpecFilename {

		var ok bool
		var specification *APISpecification

		if specification, ok = APISuite[""]; !ok || !collapse {
			specification = &APISpecification{}
		}

		err = specification.Load(specFilename, host)
		if err != nil {
			return err
		}

		if collapse {
			//specification.ID = "api"
		}

		APISuite[specification.ID] = specification
	}

	return nil
}

// -----------------------------------------------------------------------------
// Load loads API specs from the supplied host (usually local!)
func (c *APISpecification) Load(specFilename string, host string) error {

	if !strings.HasPrefix(specFilename, "/") {
		specFilename = "/" + specFilename
	}

	document, err := loadSpec("http://" + host + specFilename) // XXX Is there a confusion here between SpecDir and SpecFilename
	if err != nil {
		return err
	}

	apispec := document.Spec()

	basePath := apispec.BasePath
	basePathLen := len(basePath)
	// Ignore basepath if it is a single '/'
	if basePathLen == 1 && basePath[0] == '/' {
		basePathLen = 0
	}

	u, err := url.Parse(apispec.Schemes[0] + "://" + apispec.Host)
	if err != nil {
		return err
	}

	c.APIInfo.Description = string(github_flavored_markdown.Markdown([]byte(apispec.Info.Description)))
	c.APIInfo.Title = apispec.Info.Title

	logger.Tracef(nil, "Parse OpenAPI specification '%s'\n", c.APIInfo.Title)

	c.ID = TitleToKebab(c.APIInfo.Title)

	c.getSecurityDefinitions(apispec)
	c.getDefaultSecurity(apispec)

	methodNavByName := false // Should methods in the navigation be presented by type (GET, POST) or name (string)?
	if byname, ok := apispec.Extensions["x-navigateMethodsByName"].(bool); ok {
		methodNavByName = byname
	}

	//logger.Printf(nil, "DUMP OF ENTIRE SWAGGER SPEC\n")
	//spew.Dump(document)

	// Use the top level TAGS to order the API resources/endpoints
	// If Tags: [] is not defined, or empty, then no filtering or ordering takes place,#
	// and all API paths will be documented..
	for _, tag := range getTags(apispec) {
		logger.Tracef(nil, "  In tag loop...\n")
		// Tag matching may not be as expected if multiple paths have the same TAG (which is technically permitted)
		var ok bool

		var api *APIGroup
		groupingByTag := false

		if tag.Name != "" {
			groupingByTag = true
		}

		var name string // Will only populate if Tagging used in spec. processMethod overrides if needed.
		name = tag.Description
		if name == "" {
			name = tag.Name
		}
		logger.Tracef(nil, "    - %s\n", name)

		// If we're grouping by TAGs, then build the API at the tag level
		if groupingByTag {
			api = &APIGroup{
				ID:   TitleToKebab(name),
				Name: name,
				URL:  u,
				Info: &c.APIInfo,
				MethodNavigationByName: methodNavByName,
			}
		}

		for path, pathItem := range document.Analyzer.AllPaths() {
			logger.Tracef(nil, "    In path loop...\n")

			if basePathLen > 0 {
				path = basePath + path
			}

			// If not grouping by tag, then build the API at the path level
			if !groupingByTag {
				api = &APIGroup{
					ID:   TitleToKebab(name),
					Name: name,
					URL:  u,
					Info: &c.APIInfo,
					MethodNavigationByName: methodNavByName,
				}
			}

			var ver string
			if ver, ok = pathItem.Extensions["x-version"].(string); !ok {
				ver = "latest"
			}
			api.CurrentVersion = ver

			c.getMethods(tag, api, &api.Methods, &pathItem, path, ver) // Current version
			//c.getVersions(tag, api, pathItem.Versions, path)           // All versions

			// If API was populated (will not be if tags do not match), add to set
			if !groupingByTag && len(api.Methods) > 0 {
				logger.Tracef(nil, "    + Adding %s\n", name)
				c.APIs = append(c.APIs, *api) // All APIs (versioned within)
			}
		}

		if groupingByTag && len(api.Methods) > 0 {
			logger.Tracef(nil, "    + Adding %s\n", name)
			c.APIs = append(c.APIs, *api) // All APIs (versioned within)
		}
	}

	// Build a API map, grouping by version
	for _, api := range c.APIs {
		for v, _ := range api.Versions {
			if c.APIVersions == nil {
				c.APIVersions = make(map[string]APISet)
			}
			// Create copy of API and set Methods array to be correct for the version we are building
			napi := api
			napi.Methods = napi.Versions[v]
			napi.Versions = nil
			c.APIVersions[v] = append(c.APIVersions[v], napi) // Group APIs by version
		}
	}

	return nil
}

// -----------------------------------------------------------------------------

func getTags(specification *spec.Swagger) []spec.Tag {
	var tags []spec.Tag

	for _, tag := range specification.Tags {
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		tags = append(tags, spec.Tag{})
	}
	return tags
}

// -----------------------------------------------------------------------------

func (c *APISpecification) getVersions(tag spec.Tag, api *APIGroup, versions map[string]spec.PathItem, path string) {
	if versions == nil {
		return
	}
	api.Versions = make(map[string][]Method)

	for v, pi := range versions {
		logger.Tracef(nil, "Process version %s\n", v)
		var method []Method
		c.getMethods(tag, api, &method, &pi, path, v)
		api.Versions[v] = method
	}
}

// -----------------------------------------------------------------------------

func (c *APISpecification) getMethods(tag spec.Tag, api *APIGroup, methods *[]Method, pi *spec.PathItem, path string, version string) {

	c.getMethod(tag, api, methods, version, pi, pi.Get, path, "get")
	c.getMethod(tag, api, methods, version, pi, pi.Post, path, "post")
	c.getMethod(tag, api, methods, version, pi, pi.Put, path, "put")
	c.getMethod(tag, api, methods, version, pi, pi.Delete, path, "delete")
	c.getMethod(tag, api, methods, version, pi, pi.Head, path, "head")
	c.getMethod(tag, api, methods, version, pi, pi.Options, path, "options")
	c.getMethod(tag, api, methods, version, pi, pi.Patch, path, "patch")
}

// -----------------------------------------------------------------------------

func (c *APISpecification) getMethod(tag spec.Tag, api *APIGroup, methods *[]Method, version string, pathitem *spec.PathItem, operation *spec.Operation, path, methodname string) {
	if operation == nil {
		return
	}
	// Filter and sort by matching current top-level tag with the operation tags.
	// If Tagging is not used by spec, then process each operation without filtering.
	taglen := len(operation.Tags)
	logger.Tracef(nil, "  Operation tag length: %d", taglen)
	if taglen == 0 {
		if tag.Name != "" {
			logger.Tracef(nil, "Skipping %s - Operation does not contain a tag member, and tagging is in use.", operation.Summary)
			return
		}
		method := c.processMethod(api, pathitem, operation, path, methodname, version)
		*methods = append(*methods, *method)
	} else {
		logger.Tracef(nil, "    > Check tags")
		for _, t := range operation.Tags {
			logger.Tracef(nil, "      - Compare tag '%s' with '%s'\n", tag.Name, t)
			if tag.Name == "" || t == tag.Name {
				method := c.processMethod(api, pathitem, operation, path, methodname, version)
				*methods = append(*methods, *method)
			}
		}
	}
}

// -----------------------------------------------------------------------------

func (c *APISpecification) getSecurityDefinitions(spec *spec.Swagger) {

	if c.SecurityDefinitions == nil {
		c.SecurityDefinitions = make(map[string]SecurityScheme)
	}

	for n, d := range spec.SecurityDefinitions {
		stype := d.Type

		def := &SecurityScheme{
			Description:   string(github_flavored_markdown.Markdown([]byte(d.Description))),
			Type:          stype,  // basic, apiKey or oauth2
			ParamName:     d.Name, // name of header to be used if ParamLocation is 'header'
			ParamLocation: d.In,   // Either query or header
		}

		if stype == "apiKey" {
			def.IsApiKey = true
		}
		if stype == "basic" {
			def.IsBasic = true
		}
		if stype == "oauth2" {
			def.IsOAuth2 = true
			def.OAuth2Flow = d.Flow                   // implicit, password (explicit) application or accessCode
			def.AuthorizationUrl = d.AuthorizationURL // Only for implicit or accesscode flow
			def.TokenUrl = d.TokenURL                 // Only for implicit, accesscode or password flow
			def.Scopes = make(map[string]string)
			for s, n := range d.Scopes {
				def.Scopes[s] = n
			}
		}

		c.SecurityDefinitions[n] = *def
	}
}

// -----------------------------------------------------------------------------

func (c *APISpecification) getDefaultSecurity(spec *spec.Swagger) {
	c.DefaultSecurity = make(map[string]Security)
	c.processSecurity(spec.Security, c.DefaultSecurity)
}

// -----------------------------------------------------------------------------

func (c *APISpecification) processMethod(api *APIGroup, pathItem *spec.PathItem, o *spec.Operation, path, methodname string, version string) *Method {

	var opname string
	var gotOpname bool

	operationName := methodname
	if opname, gotOpname = o.Extensions["x-operationName"].(string); gotOpname {
		operationName = opname
	}

	// Construct an ID for the Method. Choose from operation ID, x-operationName, summary and lastly method name.
	id := o.ID // OperationID
	if id == "" {
		// No ID, use operation name
		if gotOpname {
			id = TitleToKebab(opname)
		} else {
			id = TitleToKebab(o.Summary) // No opname, use summary
			if id == "" {
				id = methodname // Last chance. Method name.
			}
		}
	}

	navigationName := operationName
	if api.MethodNavigationByName {
		navigationName = o.Summary
	}

	method := &Method{
		ID:             CamelToKebab(id),
		Name:           o.Summary,
		Description:    string(github_flavored_markdown.Markdown([]byte(o.Description))),
		Method:         methodname,
		Path:           path,
		Responses:      make(map[int]Response),
		NavigationName: navigationName,
		OperationName:  operationName,
		APIGroup:       api,
	}

	// If Tagging is not used by spec to select, group and order API paths to document, then
	// complete the missing names.
	// First try the vendor extension x-pathName, falling back to summary if not set.
	// XXX Note, that the APIGroup will get the last pathName set on the path methods added to the group (by tag).
	//
	if pathname, ok := pathItem.Extensions["x-pathName"].(string); ok {
		api.Name = pathname
		api.ID = TitleToKebab(api.Name)
	}
	if api.Name == "" {
		name := o.Summary
		if name == "" {
			logger.Errorf(nil, "Error: Operation '%s' does not have an operationId or summary member.", id)
			os.Exit(1)
		}
		api.Name = name
		api.ID = TitleToKebab(name)
	}

	if c.ResourceList == nil {
		c.ResourceList = make(map[string]map[string]*Resource)
	}

	for _, param := range o.Parameters {
		p := Parameter{
			Name:        param.Name,
			In:          param.In,
			Description: string(github_flavored_markdown.Markdown([]byte(param.Description))),
			Type:        param.Type,
			Required:    param.Required,
		}
		switch strings.ToLower(param.In) {
		case "form":
			method.FormParams = append(method.FormParams, p)
		case "path":
			method.PathParams = append(method.PathParams, p)
		case "body":
			var body map[string]interface{}
			p.Resource, body = c.resourceFromSchema(param.Schema, method, nil, true)
			p.Resource.Schema = jsonResourceToString(body, "")
			method.BodyParam = &p
		case "header":
			method.HeaderParams = append(method.HeaderParams, p)
		case "query":
			method.QueryParams = append(method.QueryParams, p)
		}
		switch strings.ToLower(param.Type) {
		case "enum":
			for _, e := range param.Enum {
				p.Enum = append(p.Enum, fmt.Sprintf("%s", e))
			}
		}
	}

	// Compile resources from response declaration
	// FIXME - Dies if there are no responses...
	for status, response := range o.Responses.StatusCodeResponses {
		var vres *Resource

		logger.Tracef(nil, "Response for status %d", status)
		//spew.Dump(response)

		// Discover if the resource is already declared, and pick it up
		// if it is (keyed on version number)
		if response.Schema != nil {
			if _, ok := c.ResourceList[version]; !ok {
				c.ResourceList[version] = make(map[string]*Resource)
			}
			var ok bool
			r, example_json := c.resourceFromSchema(response.Schema, method, nil, false) // May be thrown away

			r.Schema = jsonResourceToString(example_json, r.Type[0])

			// Look for a pre-declared resource with the response ID, and use that or create the first one...
			logger.Tracef(nil, "++ Resource version %s  ID %s\n", version, r.ID)
			if vres, ok = c.ResourceList[version][r.ID]; !ok {
				logger.Tracef(nil, "   - Creating new resource\n")
				vres = r
			}
			c.ResourceList[version][r.ID] = vres

			// Compile a list of the methods which use this resource
			vres.Methods = append(vres.Methods, *method)

			// Add the resource to the method which uses it
			method.Resources = append(method.Resources, vres)

		}

		method.Responses[status] = Response{
			Description: string(github_flavored_markdown.Markdown([]byte(response.Description))),
			Resource:    vres,
		}
	}

	if o.Responses.Default != nil {
		r, example_json := c.resourceFromSchema(o.Responses.Default.Schema, method, nil, false)
		if r != nil {

			r.Schema = jsonResourceToString(example_json, r.Type[0])

			logger.Tracef(nil, "++ Resource version %s  ID %s\n", version, r.ID)
			// Look for a pre-declared resource with the response ID, and use that or create the first one...
			var vres *Resource
			var ok bool
			if vres, ok = c.ResourceList[version][r.ID]; !ok {
				logger.Tracef(nil, "   - Creating new resource\n")
				vres = r
			}
			c.ResourceList[version][r.ID] = vres

			// Add to the compiled list of methods which use this resource
			vres.Methods = append(vres.Methods, *method)

			// Set the default response
			method.DefaultResponse = &Response{
				Description: string(github_flavored_markdown.Markdown([]byte(o.Responses.Default.Description))),
				Resource:    vres,
			}
		}
	}

	// If no Security given for operation, then the global defaults are appled.
	method.Security = make(map[string]Security)
	if c.processSecurity(o.Security, method.Security) == false {
		method.Security = c.DefaultSecurity
	}

	return method
}

// -----------------------------------------------------------------------------

func (c *APISpecification) processSecurity(s []map[string][]string, security map[string]Security) bool {

	count := 0
	for _, sec := range s {
		for n, scopes := range sec {
			// Lookup security name in definitions
			if scheme, ok := c.SecurityDefinitions[n]; ok {
				count++

				// Add security
				security[n] = Security{
					Scheme: &scheme,
					Scopes: make(map[string]string),
				}

				// Populate method specific scopes by cross referencing SecurityDefinitions
				for _, scope := range scopes {
					if scope_desc, ok := scheme.Scopes[scope]; ok {
						security[n].Scopes[scope] = scope_desc
					}
				}
			}
		}
	}
	return count != 0
}

// -----------------------------------------------------------------------------

func jsonResourceToString(jsonres map[string]interface{}, restype string) string {

	// If the resource is an array, then append json object to outer array, else serialise the object.
	var example []byte
	if strings.ToLower(restype) == "array" {
		var array_obj []map[string]interface{}
		array_obj = append(array_obj, jsonres)
		example, _ = JSONMarshalIndent(array_obj)
	} else {
		example, _ = JSONMarshalIndent(jsonres)
	}
	return string(example)
}

// -----------------------------------------------------------------------------

func checkPropertyType(s *spec.Schema) string {

	/*
	   (string) (len=12) "string_array": (spec.Schema) {
	    SchemaProps: (spec.SchemaProps) {
	     Description: (string) (len=16) "Array of strings",
	     Type: (spec.StringOrArray) (len=1 cap=1) { (string) (len=5) "array" },
	     Items: (*spec.SchemaOrArray)(0xc8205bb000)({
	      Schema: (*spec.Schema)(0xc820202480)({
	       SchemaProps: (spec.SchemaProps) {
	        Type: (spec.StringOrArray) (len=1 cap=1) { (string) (len=6) "string" },
	       },
	      }),
	     }),
	    },
	   }
	*/
	ptype := "primitive"

	if s.Type == nil {
		ptype = "object"
	}

	if s.Items != nil {
		ptype = "UNKNOWN"

		if s.Type.Contains("array") {

			if s.Items.Schema != nil {
				s = s.Items.Schema
			} else {
				s = &s.Items.Schemas[0] // - Main schema [1] = Additional properties? See online swagger editior.
			}

			if s.Type == nil {
				ptype = "array of objects"
				if s.SchemaProps.Type != nil {
					ptype = "array of SOMETHING"
				}
			} else if s.Type.Contains("array") {
				ptype = "array of primitives"
			}
		} else {
			ptype = "Some object"
		}
	}

	return ptype
}

// -----------------------------------------------------------------------------

func (c *APISpecification) resourceFromSchema(s *spec.Schema, method *Method, fqNS []string, onlyIsWritable bool) (*Resource, map[string]interface{}) {
	if s == nil {
		return nil, nil
	}

	stype := checkPropertyType(s)
	logger.Tracef(nil, "resourceFromSchema: Schema type: %s\n", stype)
	logger.Tracef(nil, "FQNS: %s\n", fqNS)
	logger.Tracef(nil, "CHECK schema type and items\n")
	//spew.Dump(s)

	// It is possible for a response to be an array of
	//     objects, and it it possible to declare this in several ways:
	// 1. As :
	//      "schema": {
	//        "$ref": "model"
	//      }
	//      Where the model declares itself of type array (of objects)
	// 2. Or :
	//    "schema": {
	//        "type": "array",
	//        "items": {
	//            "$ref": "model"
	//        }
	//    }
	//
	//  In the second version, "items" points to a schema. So what we have done to align these
	//  two cases is to keep the top level "type" in the second case, and apply it to items.schema.Type,
	//  reseting our schema variable to items.schema.

	if s.Type == nil {
		s.Type = append(s.Type, "object")
	}

	original_s := s
	if s.Items != nil {
		stringorarray := s.Type

		// Jump to nearest schema for items, depending on how it was declared
		if s.Items.Schema != nil { // items: { properties: {} }
			s = s.Items.Schema
			logger.Tracef(nil, "got s.Items.Schema for %s\n", s.Title)
		} else { // items: { $ref: "" }
			s = &s.Items.Schemas[0]
			logger.Tracef(nil, "got s.Items.Schemas[0] for %s\n", s.Title)
		}
		if s.Type == nil {
			logger.Tracef(nil, "Got array of objects or object. Name %s\n", s.Title)
			s.Type = stringorarray // Put back original type
		} else if s.Type.Contains("array") {
			logger.Tracef(nil, "Got array for %s\n", s.Title)
			s.Type = stringorarray // Put back original type
		} else if stringorarray.Contains("array") && len(s.Properties) == 0 {
			// if we get here then we can assume the type is supposed to be an array of primitives
			// Store the actual primitive type in the second element of the Type array.
			s.Type = spec.StringOrArray([]string{"array", s.Type[0]})
		}
		logger.Tracef(nil, "REMAP SCHEMA (Type is now %s)\n", s.Type)
	}

	if len(s.Format) > 0 {
		s.Type[len(s.Type)-1] = s.Format
	}

	id := TitleToKebab(s.Title)

	if len(fqNS) == 0 && id == "" {
		logger.Errorf(nil, "Error: %s %s references a model definition that does not have a title member.", strings.ToUpper(method.Method), method.Path)
		os.Exit(1)
	}

	if len(fqNS) > 0 && s.Type.Contains("array") {
		id = ""
	}

	if strings.ToLower(s.Type[0]) == "array" {
		fqNSlen := len(fqNS)
		if fqNSlen > 0 {
			fqNS = append(fqNS[0:fqNSlen-1], fqNS[fqNSlen-1]+"[]")
		}
	}

	//myFQNS := append([]string{}, fqNS...)
	myFQNS := fqNS
	var chopped bool

	if len(id) == 0 && len(myFQNS) > 0 {
		id = myFQNS[len(myFQNS)-1]
		myFQNS = append([]string{}, myFQNS[0:len(myFQNS)-1]...)
		chopped = true
		logger.Tracef(nil, "Chopped %s from myFQNS leaving %s\n", id, myFQNS)
	}

	resourceFQNS := myFQNS
	// If we are dealing with an object, then adjust the resource FQNS and id
	// so that the last element of the FQNS is chopped off and used as the ID
	if !chopped && s.Type.Contains("object") {
		if len(resourceFQNS) > 0 {
			id = resourceFQNS[len(resourceFQNS)-1]
			resourceFQNS = resourceFQNS[:len(resourceFQNS)-1]
			logger.Tracef(nil, "Got an object, so slicing %s from resourceFQNS leaving %s\n", id, myFQNS)
		}
	}

	// If there is no description... the case where we have an array of objects. See issue/11
	var description string
	if original_s.Description != "" {
		description = string(github_flavored_markdown.Markdown([]byte(original_s.Description)))
	} else {
		description = original_s.Title
	}

	logger.Tracef(nil, "Create resource %s\n", id)
	r := &Resource{
		ID:          id,
		Title:       s.Title,
		Description: description,
		Type:        s.Type,
		Properties:  make(map[string]*Resource),
		FQNS:        resourceFQNS,
	}

	if s.Example != nil {
		example, err := JSONMarshalIndent(&s.Example)
		if err != nil {
			logger.Errorf(nil, "Error encoding example json: %s", err)
		}
		r.Example = string(example)
	}

	if len(s.Enum) > 0 {
		for _, e := range s.Enum {
			r.Enum = append(r.Enum, fmt.Sprintf("%s", e))
		}
	}

	r.ReadOnly = original_s.ReadOnly
	if ops, ok := original_s.Extensions["x-excludeFromOperations"].([]interface{}); ok {
		// Mark resource property as being excluded from operations with this name.
		// This filtering only takes effect in a request body, just like readOnly.
		for _, op := range ops {
			if c, ok := op.(string); ok {
				r.ExcludeFromOperations = append(r.ExcludeFromOperations, c)
			}
		}
	}

	required := make(map[string]bool)
	json_representation := make(map[string]interface{})

	logger.Tracef(nil, "Call compileproperties...\n")
	c.compileproperties(s, r, method, id, required, json_representation, myFQNS, chopped, onlyIsWritable)

	for allof := range s.AllOf {
		c.compileproperties(&s.AllOf[allof], r, method, id, required, json_representation, myFQNS, chopped, onlyIsWritable)
	}

	logger.Tracef(nil, "resourceFromSchema done\n")

	return r, json_representation
}

// -----------------------------------------------------------------------------
// Takes a Schema object and adds properties to the Resource object.
// It uses the 'required' map to set when properties are required and builds a JSON
// representation of the resource.
//
func (c *APISpecification) compileproperties(s *spec.Schema, r *Resource, method *Method, id string, required map[string]bool, json_rep map[string]interface{}, myFQNS []string, chopped bool, onlyIsWritable bool) {

	// First, grab the required members
	for _, n := range s.Required {
		required[n] = true
	}

	for name, property := range s.Properties {
		c.processProperty(&property, name, r, method, id, required, json_rep, myFQNS, chopped, onlyIsWritable)
	}

	// Special case to deal with AdditionalProperties (which really just boils down to declaring a
	// map of 'type' (string, int, object etc).
	if s.AdditionalProperties != nil && s.AdditionalProperties.Allows {
		name := "<key>"
		ap := s.AdditionalProperties.Schema
		ap.Type = spec.StringOrArray([]string{"map", ap.Type[0]}) // massage type so that it is a map of 'type'

		c.processProperty(ap, name, r, method, id, required, json_rep, myFQNS, chopped, onlyIsWritable)
	}
}

// -----------------------------------------------------------------------------

func (c *APISpecification) processProperty(s *spec.Schema, name string, r *Resource, method *Method, id string, required map[string]bool, json_rep map[string]interface{}, myFQNS []string, chopped bool, onlyIsWritable bool) {

	newFQNS := prepareNamespace(myFQNS, id, name, chopped)

	var json_resource map[string]interface{}
	var resource *Resource

	logger.Tracef(nil, "A call resourceFromSchema for property %s\n", name)
	resource, json_resource = c.resourceFromSchema(s, method, newFQNS, onlyIsWritable)

	skip := onlyIsWritable && resource.ReadOnly
	if !skip && resource.ExcludeFromOperations != nil {
		for _, opname := range resource.ExcludeFromOperations {
			if opname == method.OperationName {
				skip = true
				break
			}
		}
	}
	if skip {
		return
	}

	r.Properties[name] = resource
	json_rep[name] = json_resource

	if _, ok := required[name]; ok {
		r.Properties[name].Required = true
	}
	logger.Tracef(nil, "resource property %s type: %s\n", name, r.Properties[name].Type[0])

	if strings.ToLower(r.Properties[name].Type[0]) != "object" {
		// Arrays of objects need to be handled as a special case
		if strings.ToLower(r.Properties[name].Type[0]) == "array" {
			logger.Tracef(nil, "Processing an array property %s", name)
			if s.Items != nil {
				if s.Items.Schema != nil {
					// Some outputs (example schema, member description) are generated differently
					// if the array member references an object or a primitive type
					r.Properties[name].Description = s.Description

					// If here, we have no json_resource returned from resourceFromSchema, then the property
					// is an array of primitive, so construct either an array of string or array of object
					// as appropriate.
					if len(json_resource) > 0 {
						var array_obj []map[string]interface{}
						array_obj = append(array_obj, json_resource)
						json_rep[name] = array_obj
					} else {
						var array_obj []string
						// We stored the real type of the primitive in Type array index 1 (see the note in
						// resourceFromSchema).
						array_obj = append(array_obj, r.Properties[name].Type[1])
						json_rep[name] = array_obj
					}
				} else { // array and property.Items.Schema is NIL
					var array_obj []map[string]interface{}
					array_obj = append(array_obj, json_resource)
					json_rep[name] = array_obj
				}
			} else { // array and Items are nil
				var array_obj []map[string]interface{}
				array_obj = append(array_obj, json_resource)
				json_rep[name] = array_obj
			}
		} else if strings.ToLower(r.Properties[name].Type[0]) == "map" { // not array, so a map?
			if strings.ToLower(r.Properties[name].Type[1]) == "object" {
				json_rep[name] = json_resource // A map of objects
			} else {
				json_rep[name] = r.Properties[name].Type[1] // map of primitive
			}
		} else {
			// We're NOT an array, map or object, so a primitive
			json_rep[name] = r.Properties[name].Type[0]
		}
	} else {
		// We're an object
		json_rep[name] = json_resource
	}
	return
}

// -----------------------------------------------------------------------------

func prepareNamespace(myFQNS []string, id string, name string, chopped bool) []string {

	newFQNS := append([]string{}, myFQNS...) // create slice

	if chopped && len(id) > 0 {
		logger.Tracef(nil, "Append ID onto newFQNZ %s + '%s'", newFQNS, id)
		newFQNS = append(newFQNS, id)
	}

	newFQNS = append(newFQNS, name)

	return newFQNS
}

// -----------------------------------------------------------------------------

func TitleToKebab(s string) string {
	s = strings.ToLower(s)
	s = strings.Replace(s, " ", "-", -1)
	return s
}

// -----------------------------------------------------------------------------

func CamelToKebab(s string) string {
	s = snaker.CamelToSnake(s)
	s = strings.Replace(s, "_", "-", -1)
	return s
}

// -----------------------------------------------------------------------------

func loadSpec(url string) (*loads.Document, error) {
	document, err := loads.Spec(url)
	if err != nil {
		return nil, err
	}

	err = spec.ExpandSpec(document.Spec())
	if err != nil {
		return nil, err
	}

	return document, nil
}

// -----------------------------------------------------------------------------
// Wrapper around MarshalIndent to prevent < > & from being escaped
func JSONMarshalIndent(v interface{}) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "    ")

	b = bytes.Replace(b, []byte("\\u003c"), []byte("<"), -1)
	b = bytes.Replace(b, []byte("\\u003e"), []byte(">"), -1)
	b = bytes.Replace(b, []byte("\\u0026"), []byte("&"), -1)
	return b, err
}

// -----------------------------------------------------------------------------
