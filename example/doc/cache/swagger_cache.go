package cache

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/client/genericclient"
	"github.com/cloudwego/kitex/pkg/generic"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/transport"
	"github.com/hertz-contrib/cors"
	"github.com/hertz-contrib/swagger"
	swaggerFiles "github.com/swaggo/files"
)

//go:embed ../openapi.yaml
var openapiYAML []byte

var clientCache sync.Map
var cacheTime = make(map[genericclient.Client]time.Time)
var cacheMutex sync.Mutex
var cacheTimeout = 30 * time.Minute

func main() {
	h := server.Default(server.WithHostPorts("127.0.0.1:8080"))

	h.Use(cors.Default())

	setupSwaggerRoutes(h)
	setupProxyRoutes(h)

	hlog.Info("Swagger UI is available at: http://127.0.0.1:8080/swagger/index.html")

	h.Spin()
}

func findThriftFile(fileName string) (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	foundPath := ""
	err = filepath.Walk(workingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == fileName {
			foundPath = path
			return filepath.SkipDir
		}
		return nil
	})

	if err == nil && foundPath != "" {
		return foundPath, nil
	}

	parentDir := filepath.Dir(workingDir)
	for parentDir != "/" && parentDir != "." && parentDir != workingDir {
		filePath := filepath.Join(parentDir, fileName)
		if _, err := os.Stat(filePath); err == nil {
			return filePath, nil
		}
		workingDir = parentDir
		parentDir = filepath.Dir(parentDir)
	}

	return "", errors.New("thrift file not found: " + fileName)
}

func initializeGenericClient(svcName string) (genericclient.Client, error) {
	thriftFile, err := findThriftFile(svcName+".thrift")
	if err != nil {
		hlog.Fatal("Failed to locate Thrift file:", err)
	}

	p, err := generic.NewThriftFileProvider(thriftFile)
	if err != nil {
		hlog.Fatal("Failed to create ThriftFileProvider:", err)
	}

	g, err := generic.JSONThriftGeneric(p)
	if err != nil {
		hlog.Fatal("Failed to create HTTPThriftGeneric:", err)
	}

	var opts []client.Option
	opts = append(opts, client.WithTransportProtocol(transport.TTHeader))
	opts = append(opts, client.WithMetaHandler(transmeta.ClientTTHeaderHandler))
	opts = append(opts, client.WithHostPorts("127.0.0.1:8888"))

	cli, err := genericclient.NewClient(svcName, g, opts...)
	if err != nil {
		return nil, err
	}

	return cli, nil
}

func GetCacheClient(svcName string) (genericclient.Client, error) {
	if newClient, ok := clientCache.Load(svcName); ok {
		oldClient := newClient.(genericclient.Client)

		cacheMutex.Lock()
		defer cacheMutex.Unlock()

		if time.Since(cacheTime[oldClient]) < cacheTimeout {
			// 缓存未过期，直接返回
			return oldClient, nil
		}
		// 缓存已过期，删除旧缓存
		clientCache.Delete(svcName)
	}

	// 缓存不存在，创建新缓存
	newClient, err := initializeGenericClient(svcName)
	if err != nil {
		return nil, err
	}

	cacheMutex.Lock()
	cacheTime[newClient] = time.Now() // 记录缓存时间
	cacheMutex.Unlock()

	clientCache.Store(svcName, newClient)
	return newClient, nil
}

func setupSwaggerRoutes(h *server.Hertz) {
	h.GET("swagger/*any", swagger.WrapHandler(swaggerFiles.Handler, swagger.URL("/openapi.yaml")))

	h.GET("/openapi.yaml", func(c context.Context, ctx *app.RequestContext) {
		ctx.Header("Content-Type", "application/x-yaml")
		ctx.Write(openapiYAML)
	})
}

func setupProxyRoutes(h *server.Hertz) {
	h.Any("/*ServiceMethod", func(c context.Context, ctx *app.RequestContext) {
		serviceMethod := ctx.Param("ServiceMethod")
		if serviceMethod == "" {
			handleError(ctx, "ServiceMethod not provided", http.StatusBadRequest)
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

		cli, err := GetCacheClient("swagger")
		if err != nil {
			handleError(ctx, "Failed to get client: "+err.Error(), http.StatusInternalServerError)
			return
		}

		jReq := string(bodyBytes)

		jRsp, err := cli.GenericCall(c, serviceMethod, jReq)
		if err != nil {
			hlog.Errorf("GenericCall error: %v", err)
			ctx.JSON(500, map[string]interface{}{
				"error": err.Error(),
			})
			return
		}

		result := make(map[string]interface{})
		if err := json.Unmarshal([]byte(jRsp.(string)), &result); err != nil {
			hlog.Errorf("Failed to unmarshal response body: %v", err)
			ctx.JSON(500, map[string]interface{}{
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
			ctx.JSON(500, map[string]interface{}{
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
