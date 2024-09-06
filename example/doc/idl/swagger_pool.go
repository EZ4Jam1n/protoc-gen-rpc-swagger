package main

import (
	"context"
	"embed"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/bytedance/gopkg/cloud/metainfo"
	dproto "github.com/cloudwego/dynamicgo/proto"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/client/genericclient"
	"github.com/cloudwego/kitex/pkg/generic"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/transport"
	"github.com/emicklei/proto"
	"github.com/hertz-contrib/cors"
	"github.com/hertz-contrib/swagger"
	swaggerFiles "github.com/swaggo/files"
)

//go:embed openapi.yaml
var openapiYAML []byte

// {{根据source_relative来判断}}
//
//go:embed *.openapi.yaml openapi.yaml
var files embed.FS

// ClientPool is a map of service name to client
type ClientPool struct {
	serviceMap map[string]genericclient.Client
	mutex      sync.Mutex
}

// NewClientPool creates a new client pool
func NewClientPool(protoFiles []string) *ClientPool {
	clientPool := &ClientPool{
		serviceMap: make(map[string]genericclient.Client),
	}

	for _, protoFile := range protoFiles {
		err := clientPool.GetServicesFromIDL(protoFile)
		if err != nil {
			hlog.Fatalf("Error loading thrift files from directory: %v", err)
		}
	}

	return clientPool
}

// newClient creates a new client for the specified service
func newClient(pbFilePath, svcName string) genericclient.Client {
	dOpts := dproto.Options{}
	hlog.Info(pbFilePath)
	p, err := generic.NewPbFileProviderWithDynamicGo(pbFilePath, context.Background(), dOpts)
	if err != nil {
		hlog.Fatalf("Failed to create protobufFileProvider for %s: %v", svcName, err)
	}

	g, err := generic.JSONPbGeneric(p)
	if err != nil {
		hlog.Fatalf("Failed to create JSONPbGeneric for %s: %v", svcName, err)
	}

	cli, err := genericclient.NewClient(svcName, g,
		client.WithTransportProtocol(transport.TTHeader),
		client.WithMetaHandler(transmeta.ClientTTHeaderHandler),
		client.WithHostPorts("127.0.0.1:8888"),
	)
	if err != nil {
		hlog.Fatalf("Failed to create generic client for %s: %v", svcName, err)
	}

	hlog.Infof("Created new client for service: %s", svcName)
	return cli
}

// getClient returns the client for the specified service name
func (cp *ClientPool) getClient(svcName string) (genericclient.Client, error) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	client, ok := cp.serviceMap[svcName]
	if !ok {
		return nil, errors.New("service not found: " + svcName)
	}
	return client, nil
}

// GetServicesFromIDL 解析 .proto 文件并从中提取服务定义
func (cp *ClientPool) GetServicesFromIDL(idlPath string) error {
	reader, err := os.Open(idlPath)
	if err != nil {
		return fmt.Errorf("failed to open proto file: %w", err)
	}
	defer reader.Close()

	parser := proto.NewParser(reader)
	definition, err := parser.Parse()
	if err != nil {
		return fmt.Errorf("failed to parse proto file: %w", err)
	}

	proto.Walk(definition,
		proto.WithService(func(s *proto.Service) {
			cp.serviceMap[s.Name] = newClient(idlPath, s.Name)
		}),
	)
	hlog.Info(cp.serviceMap)
	return nil
}

func main() {
	h := server.Default(server.WithHostPorts("127.0.0.1:8080"))
	h.Use(cors.Default())

	protoFiles := []string{
		"./hello.proto",
		"./hello2.proto",
	}
	// Initialize the client pool with the directory containing Thrift files
	clientPool := NewClientPool(protoFiles)

	setupSwaggerRoutes(h)
	setupProxyRoutes(h, clientPool)

	hlog.Info("Swagger UI is available at: http://127.0.0.1:8080/swagger/index.html")
	h.Spin()
}

func setupSwaggerRoutes(h *server.Hertz) {
	h.GET("swagger/*any", swagger.WrapHandler(swaggerFiles.Handler, swagger.URL("/openapi.yaml")))

	h.GET("/openapi.yaml", func(c context.Context, ctx *app.RequestContext) {
		ctx.Header("Content-Type", "application/x-yaml")
		ctx.Write(openapiYAML)
	})
}

func setupProxyRoutes(h *server.Hertz, cp *ClientPool) {
	h.Any("/*ServiceMethod", func(c context.Context, ctx *app.RequestContext) {
		serviceMethod := ctx.Param("ServiceMethod")
		if serviceMethod == "" {
			handleError(ctx, "ServiceMethod not provided", http.StatusBadRequest)
			return
		}

		// Split the service and method from the path
		parts := strings.SplitN(serviceMethod, ".", 2)
		if len(parts) < 2 {
			handleError(ctx, "Invalid ServiceMethod format, expected svcName.MethodName", http.StatusBadRequest)
			return
		}

		svcName := parts[0]
		methodName := parts[1]

		cli, err := cp.getClient(svcName)
		if err != nil {
			handleError(ctx, err.Error(), http.StatusNotFound)
			return
		}

		bodyBytes := ctx.Request.Body()

		queryMap := formatQueryParams(ctx)

		for k, v := range queryMap {
			if strings.HasPrefix(k, "p_") {
				c = metainfo.WithPersistentValue(c, k, v)
			} else {
				c = metainfo.WithValue(c, k, v)
			}
		}

		c = metainfo.WithBackwardValues(c)

		jReq := string(bodyBytes)

		jRsp, err := cli.GenericCall(c, methodName, jReq)
		if err != nil {
			hlog.Errorf("GenericCall error: %v", err)
			ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"error": err.Error(),
			})
			return
		}

		result := make(map[string]interface{})
		if err := json.Unmarshal([]byte(jRsp.(string)), &result); err != nil {
			hlog.Errorf("Failed to unmarshal response body: %v", err)
			ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"error": "Failed to unmarshal response body",
			})
			return
		}

		m := metainfo.RecvAllBackwardValues(c)

		for key, value := range m {
			result[key] = value
		}

		respBody, err := json.Marshal(result)
		if err != nil {
			hlog.Errorf("Failed to marshal response body: %v", err)
			ctx.JSON(http.StatusInternalServerError, map[string]interface{}{
				"error": "Failed to marshal response body",
			})
			return
		}

		ctx.Data(http.StatusOK, "application/json", respBody)
	})
}

func formatQueryParams(ctx *app.RequestContext) map[string]string {
	var QueryParams = make(map[string]string)
	ctx.Request.URI().QueryArgs().VisitAll(func(key, value []byte) {
		QueryParams[string(key)] = string(value)
	})
	return QueryParams
}

func handleError(ctx *app.RequestContext, errMsg string, statusCode int) {
	hlog.Errorf("Error: %s", errMsg)
	ctx.JSON(statusCode, map[string]interface{}{
		"error": errMsg,
	})
}
