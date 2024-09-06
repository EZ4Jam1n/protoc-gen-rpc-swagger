/*
 * Copyright 2024 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package generator

import (
	"bytes"
	"fmt"
	"path/filepath"
	"text/template"

	"github.com/cloudwego/hertz/cmd/hz/util/logs"
	"google.golang.org/protobuf/compiler/protogen"
)

type ServerConfiguration struct {
	HertzAddr *string
	KitexAddr *string
}

type ServerGenerator struct {
	IdlPath   string
	HertzAddr string
	KitexAddr string
	OutputDir string
}

func NewServerGenerator(conf ServerConfiguration) *ServerGenerator {
	defaultHertzAddr := DefaultHertzAddr
	defaultKitexAddr := DefaultKitexAddr

	if hertzAddr := conf.HertzAddr; hertzAddr != nil {
		*hertzAddr = defaultHertzAddr
	}

	if kitexAddr := conf.KitexAddr; kitexAddr != nil {
		*kitexAddr = defaultKitexAddr
	}

	return &ServerGenerator{
		IdlPath:   "hello.proto",
		HertzAddr: *conf.HertzAddr,
		KitexAddr: *conf.KitexAddr,
	}
}

func (g *ServerGenerator) Generate(outputFile *protogen.GeneratedFile) error {
	tmpl, err := template.New("server").Delims("{{", "}}").Parse(serverTemplate)
	if err != nil {
		logs.Errorf("failed to parse template: %v", err)
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, g)
	if err != nil {
		logs.Errorf("failed to execute template: %v", err)
	}

	filePath := filepath.Clean(g.OutputDir)
	filePath = filepath.Join(filePath, DefaultOutputSwaggerFile)
	if _, err = outputFile.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write output file: %v", err)
	}
	return nil
}

const (
	DefaultHertzAddr         = "127.0.0.1:8080"
	DefaultKitexAddr         = "127.0.0.1:8888"
	DefaultOutputSwaggerFile = "swagger.go"
)

const serverTemplate = `// Code generated by thrift-gen-rpc-swagger.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

//go:embed openapi.yaml
var openapiYAML []byte

func main() {
	h := server.Default(server.WithHostPorts("{{.HertzAddr}}"))

	h.Use(cors.Default())

	cli := initializeGenericClient()
	setupSwaggerRoutes(h)
	setupProxyRoutes(h, cli)

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

func initializeGenericClient() genericclient.Client {
	thriftFile, err := findThriftFile("{{.IdlPath}}")
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
	opts = append(opts, client.WithHostPorts("{{.KitexAddr}}"))
	cli, err := genericclient.NewClient("swagger", g, opts...)
	if err != nil {
		hlog.Fatal("Failed to create generic client:", err)
	}

	return cli
}

func setupSwaggerRoutes(h *server.Hertz) {
	h.GET("swagger/*any", swagger.WrapHandler(swaggerFiles.Handler, swagger.URL("/openapi.yaml")))

	h.GET("/openapi.yaml", func(c context.Context, ctx *app.RequestContext) {
		ctx.Header("Content-Type", "application/x-yaml")
		ctx.Write(openapiYAML)
	})
}

func setupProxyRoutes(h *server.Hertz, cli genericclient.Client) {
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
`
