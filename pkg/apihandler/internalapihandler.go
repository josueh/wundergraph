package apihandler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/golang-jwt/jwt/v4"
	"github.com/gorilla/mux"
	"github.com/jensneuse/abstractlogger"
	"github.com/wundergraph/graphql-go-tools/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/pkg/astparser"
	"github.com/wundergraph/graphql-go-tools/pkg/asttransform"
	"github.com/wundergraph/graphql-go-tools/pkg/astvalidation"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/plan"
	"github.com/wundergraph/graphql-go-tools/pkg/engine/resolve"
	"github.com/wundergraph/wundergraph/pkg/engineconfigloader"
	"github.com/wundergraph/wundergraph/pkg/pool"
	"github.com/wundergraph/wundergraph/types/go/wgpb"
)

type InternalBuilder struct {
	pool            *pool.Pool
	log             abstractlogger.Logger
	loader          *engineconfigloader.EngineConfigLoader
	api             *wgpb.Api
	planConfig      plan.Configuration
	resolver        *resolve.Resolver
	definition      *ast.Document
	router          *mux.Router
	secret          []byte
	renameTypeNames []resolve.RenameTypeName
}

func NewInternalBuilder(pool *pool.Pool, log abstractlogger.Logger, loader *engineconfigloader.EngineConfigLoader) *InternalBuilder {
	return &InternalBuilder{
		pool:   pool,
		log:    log,
		loader: loader,
	}
}

func (i *InternalBuilder) BuildAndMountInternalApiHandler(ctx context.Context, router *mux.Router, api *wgpb.Api, secret []byte) (streamClosers []chan struct{}, err error) {

	if api.EngineConfiguration == nil {
		return streamClosers, fmt.Errorf("engine config must not be nil")
	}
	if api.AuthenticationConfig == nil ||
		api.AuthenticationConfig.Hooks == nil {
		return streamClosers, fmt.Errorf("authentication config missing")
	}

	i.secret = secret

	planConfig, err := i.loader.Load(*api.EngineConfiguration)
	if err != nil {
		return streamClosers, err
	}

	i.api = api
	i.planConfig = *planConfig
	i.resolver = resolve.New(ctx, resolve.NewFetcher(true), true)

	definition, report := astparser.ParseGraphqlDocumentString(api.EngineConfiguration.GraphqlSchema)
	if report.HasErrors() {
		return streamClosers, report
	}
	i.definition = &definition
	err = asttransform.MergeDefinitionWithBaseSchema(i.definition)
	if err != nil {
		return streamClosers, err
	}

	i.log.Debug("configuring API",
		abstractlogger.String("name", api.PathPrefix),
		abstractlogger.Int("numOfOperations", len(api.Operations)),
	)

	route := router.NewRoute()
	route.PathPrefix(fmt.Sprintf("/%s", api.PathPrefix))

	i.router = route.Subrouter()

	// RenameTo is the correct name for the origin
	// for the downstream (client), we have to reverse the __typename fields
	// this is why Types.RenameTo is assigned to rename.From
	for _, configuration := range planConfig.Types {
		i.renameTypeNames = append(i.renameTypeNames, resolve.RenameTypeName{
			From: []byte(configuration.RenameTo),
			To:   []byte(configuration.TypeName),
		})
	}

	for _, operation := range api.Operations {
		err = i.registerOperation(operation)
		if err != nil {
			i.log.Error("registerOperation", abstractlogger.Error(err))
		}
	}

	return streamClosers, err
}

func (i *InternalBuilder) registerOperation(operation *wgpb.Operation) error {

	shared := i.pool.GetShared(context.Background(), i.planConfig, pool.Config{
		RenameTypeNames: i.renameTypeNames,
	})

	shared.Doc.Input.ResetInputString(operation.Content)
	shared.Parser.Parse(shared.Doc, shared.Report)

	if shared.Report.HasErrors() {
		return shared.Report
	}

	shared.Normalizer.NormalizeNamedOperation(shared.Doc, i.definition, []byte(operation.Name), shared.Report)

	state := shared.Validation.Validate(shared.Doc, i.definition, shared.Report)
	if state != astvalidation.Valid {
		return shared.Report
	}

	preparedPlan := shared.Planner.Plan(shared.Doc, i.definition, operation.Name, shared.Report)
	shared.Postprocess.Process(preparedPlan)

	apiPath := fmt.Sprintf("/operations/%s", operation.Name)
	operationType := getOperationType(shared.Doc, i.definition, operation.Name)

	switch operationType {
	case ast.OperationTypeQuery,
		ast.OperationTypeMutation:
		p, ok := preparedPlan.(*plan.SynchronousResponsePlan)
		if !ok {
			return nil
		}

		extractedVariables := make([]byte, len(shared.Doc.Input.Variables))
		copy(extractedVariables, shared.Doc.Input.Variables)

		handler := &InternalApiHandler{
			preparedPlan:       p,
			operation:          operation,
			extractedVariables: extractedVariables,
			log:                i.log,
			resolver:           i.resolver,
			jwtSecret:          i.secret,
			renameTypeNames:    i.renameTypeNames,
		}

		i.router.Methods(http.MethodPost).Path(apiPath).Handler(handler)
	}

	return nil
}

func GenSymmetricKey(bits int) (k []byte, err error) {
	if bits <= 0 || bits%8 != 0 {
		return nil, fmt.Errorf("key size invalid")
	}

	size := bits / 8
	k = make([]byte, size)
	if _, err = rand.Read(k); err != nil {
		return nil, err
	}

	return k, nil
}

func CreateHooksJWT(secret []byte) (string, error) {
	token := jwt.New(jwt.SigningMethodHS256)
	return token.SignedString(secret)
}

type InternalApiHandler struct {
	preparedPlan       *plan.SynchronousResponsePlan
	operation          *wgpb.Operation
	extractedVariables []byte
	log                abstractlogger.Logger
	resolver           *resolve.Resolver
	jwtSecret          []byte
	bearerCache        sync.Map
	renameTypeNames    []resolve.RenameTypeName
}

func (h *InternalApiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	r = setOperationMetaData(r, h.operation)

	authorizationHeader := r.Header.Get("X-WG-Authorization")
	if authorizationHeader == "" { // for legacy reasons, can be removed in the future
		authorizationHeader = r.Header.Get("Authorization")
	}
	_, ok := h.bearerCache.Load(authorizationHeader)
	if !ok {
		bearer := strings.TrimPrefix(authorizationHeader, "Bearer ")
		token, err := jwt.Parse(bearer, func(token *jwt.Token) (interface{}, error) {
			return h.jwtSecret, nil
		})
		if err != nil || !token.Valid {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.bearerCache.Store(authorizationHeader, struct{}{})
	}

	variablesBuf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(variablesBuf)

	ctx := pool.GetCtx(r, pool.Config{
		RenameTypeNames: h.renameTypeNames,
	})
	defer pool.PutCtx(ctx)

	_, err := io.Copy(variablesBuf, r.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx.Variables = variablesBuf.Bytes()

	if len(ctx.Variables) == 0 {
		ctx.Variables = []byte("{}")
	}

	compactBuf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(compactBuf)
	err = json.Compact(compactBuf, ctx.Variables)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx.Variables = compactBuf.Bytes()

	if len(h.extractedVariables) != 0 {
		ctx.Variables = MergeJsonRightIntoLeft(ctx.Variables, h.extractedVariables)
	}

	ctx.Variables = injectVariables(h.operation, r, ctx.Variables)

	buf := pool.GetBytesBuffer()
	defer pool.PutBytesBuffer(buf)

	resolveErr := h.resolver.ResolveGraphQLResponse(ctx, h.preparedPlan.Response, nil, buf)
	if resolveErr != nil {
		h.log.Error("InternalApiHandler.ResolveGraphQLResponse", abstractlogger.Error(resolveErr))
		http.Error(w, "unable to resolve", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = buf.WriteTo(w)
}
