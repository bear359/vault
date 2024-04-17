// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	ctconfig "github.com/hashicorp/consul-template/config"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	agentConfig "github.com/hashicorp/vault/command/agent/config"
	"github.com/hashicorp/vault/command/agent/template"
	"github.com/hashicorp/vault/command/agentproxyshared/auth"
	tokenfile "github.com/hashicorp/vault/command/agentproxyshared/auth/token-file"
	"github.com/hashicorp/vault/command/agentproxyshared/sink"
	"github.com/hashicorp/vault/command/agentproxyshared/sink/file"
	"github.com/hashicorp/vault/helper/testhelpers/corehelpers"
	"github.com/hashicorp/vault/helper/testhelpers/minimal"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/sdk/helper/logging"
	"github.com/hashicorp/vault/sdk/helper/pointerutil"
	"github.com/hashicorp/vault/vault"
	"github.com/stretchr/testify/require"
)

const (
	lookupSelfTemplateContents = `{{ with secret "auth/token/lookup-self" }}{{ .Data.id }}{{ end }}`

	kvDataTemplateContents = `"{{ with secret "secret/data/otherapp" }}{{ .Data.data.username }}{{ end }}"`

	kvAccessPolicy = `
path "/kv/*" {
	capabilities = ["create", "read", "update", "delete", "list"]
}
path "/secret/*" {
	capabilities = ["create", "read", "update", "delete", "list"]
}`
)

// TestAutoAuthSelfHealing_TokenFileAuth_SinkOutput tests that
// if the token is revoked, Auto Auth is re-triggered and a valid new token
// is written to a sink, and the template is correctly rendered with the new token
func TestAutoAuthSelfHealing_TokenFileAuth_SinkOutput(t *testing.T) {
	// Unset the environment variable so that agent picks up the right test cluster address
	t.Setenv(api.EnvVaultAddress, "")

	tmpDir := t.TempDir()
	pathLookupSelf := filepath.Join(tmpDir, "lookup-self")
	pathVaultToken := filepath.Join(tmpDir, "vault-token")
	pathTokenFile := filepath.Join(tmpDir, "token-file")

	secretRenderInterval := 1 * time.Second
	contextTimeout := 30 * time.Second

	cluster := minimal.NewTestSoloCluster(t, nil)
	logger := corehelpers.NewTestLogger(t)
	serverClient := cluster.Cores[0].Client

	// Create token
	secret, err := serverClient.Auth().Token().Create(&api.TokenCreateRequest{
		Policies: []string{"test-autoauth"},
	})
	require.NoError(t, err)
	require.NotNil(t, secret)
	require.NotNil(t, secret.Auth)
	require.NotEmpty(t, secret.Auth.ClientToken)
	token := secret.Auth.ClientToken

	// Write token to vault-token file
	tokenFile, err := os.Create(pathVaultToken)
	require.NoError(t, err)
	_, err = tokenFile.WriteString(token)
	require.NoError(t, err)
	err = tokenFile.Close()
	require.NoError(t, err)

	// Give us some leeway of 3 errors 1 from each of: auth handler, sink server template server.
	errCh := make(chan error, 3)
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)

	// Create auth handler
	am, err := tokenfile.NewTokenFileAuthMethod(&auth.AuthConfig{
		Logger: logger.Named("auth.method"),
		Config: map[string]interface{}{
			"token_file_path": pathVaultToken,
		},
	})
	require.NoError(t, err)

	ahConfig := &auth.AuthHandlerConfig{
		Logger:                       logger.Named("auth.handler"),
		Client:                       serverClient,
		EnableExecTokenCh:            true,
		EnableTemplateTokenCh:        true,
		EnableReauthOnNewCredentials: true,
		ExitOnError:                  false,
	}
	ah := auth.NewAuthHandler(ahConfig)
	go func() {
		errCh <- ah.Run(ctx, am)
	}()

	// Create sink file server
	_, err = os.Create(pathTokenFile)
	require.NoError(t, err)

	config := &sink.SinkConfig{
		Logger: logger.Named("sink.file"),
		Config: map[string]interface{}{
			"path": pathTokenFile,
		},
	}
	fs, err := file.NewFileSink(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Sink = fs

	ss := sink.NewSinkServer(&sink.SinkServerConfig{
		Logger: logger.Named("sink.server"),
		Client: serverClient,
	})
	go func() {
		errCh <- ss.Run(ctx, ah.OutputCh, []*sink.SinkConfig{config}, ah.AuthInProgress)
	}()

	// Create template server
	sc := &template.ServerConfig{
		Logger: logger.Named("template.server"),
		AgentConfig: &agentConfig.Config{
			Vault: &agentConfig.Vault{
				Address:       serverClient.Address(),
				TLSSkipVerify: true,
			},
			TemplateConfig: &agentConfig.TemplateConfig{
				StaticSecretRenderInt: secretRenderInterval,
			},
			AutoAuth: &agentConfig.AutoAuth{
				Sinks: []*agentConfig.Sink{{Type: "file", Config: map[string]interface{}{
					"path": pathLookupSelf,
				}}},
			},
			ExitAfterAuth: false,
		},
		LogLevel:      hclog.Trace,
		LogWriter:     hclog.DefaultOutput,
		ExitAfterAuth: false,
	}

	templateTest := &ctconfig.TemplateConfig{
		Contents:    pointerutil.StringPtr(lookupSelfTemplateContents),
		Destination: pointerutil.StringPtr(pathLookupSelf),
	}
	templatesToRender := []*ctconfig.TemplateConfig{templateTest}

	var server *template.Server
	server = template.NewServer(sc)
	go func() {
		errCh <- server.Run(ctx, ah.TemplateTokenCh, templatesToRender, ah.AuthInProgress, ah.InvalidToken)
	}()

	// Trigger template render (mark the time as being earlier, based on the render interval)
	preTriggerTime := time.Now().Add(-secretRenderInterval)
	ah.TemplateTokenCh <- token
	fileInfo, err := waitForFiles(t, pathTokenFile, preTriggerTime)
	require.NoError(t, err)

	tokenInSink, err := os.ReadFile(pathTokenFile)
	require.NoError(t, err)
	require.Equal(t, token, string(tokenInSink))

	// Revoke Token
	t.Logf("revoking token")
	err = serverClient.Auth().Token().RevokeOrphan(token)
	require.NoError(t, err)

	// Create new token
	tokenSecret, err := serverClient.Auth().Token().Create(&api.TokenCreateRequest{})
	require.NoError(t, err)
	require.NotNil(t, tokenSecret)
	require.NotNil(t, tokenSecret.Auth)
	require.NotEmpty(t, tokenSecret.Auth.ClientToken)
	newToken := tokenSecret.Auth.ClientToken

	// Write token to file
	err = os.WriteFile(pathVaultToken, []byte(newToken), 0o600)
	require.NoError(t, err)

	// Wait for auto-auth to complete
	updatedFileInfo, err := waitForFiles(t, pathTokenFile, fileInfo.ModTime())
	require.NoError(t, err)

	// Verify the new token has been written to a file sink after re-authenticating using lookup-self
	tokenInSink, err = os.ReadFile(pathTokenFile)
	require.NoError(t, err)
	require.Equal(t, newToken, string(tokenInSink))

	// Wait for the lookup-self file to be updated (again)
	_, err = waitForFiles(t, pathLookupSelf, updatedFileInfo.ModTime())
	require.NoError(t, err)

	// Verify the template has now been correctly rendered with the new token
	templateContents, err := os.ReadFile(pathLookupSelf)
	require.NoError(t, err)
	require.Equal(t, newToken, string(templateContents))

	// Calling cancel will stop the 'Run' funcs we started in Goroutines, we should
	// then check that there were no errors in our channel.
	cancel()
	wrapUpTimeout := 5 * time.Second
	for {
		select {
		case <-time.After(wrapUpTimeout):
			t.Fatal("test timed out")
		case err := <-errCh:
			require.NoError(t, err)
		case <-ctx.Done():
			// We can finish the test ourselves
			return
		}
	}
}

// Test_NoAutoAuthSelfHealing_BadPolicy tests that auto auth
// is not re-triggered if a token with incorrect policy access
// is used to render a template
func Test_NoAutoAuthSelfHealing_BadPolicy(t *testing.T) {
	logger := logging.NewVaultLogger(hclog.Trace)
	cluster := vault.NewTestCluster(t,
		&vault.CoreConfig{},
		&vault.TestClusterOptions{
			NumCores:    1,
			HandlerFunc: vaulthttp.Handler,
		})
	cluster.Start()
	defer cluster.Cleanup()

	vault.TestWaitActive(t, cluster.Cores[0].Core)
	serverClient := cluster.Cores[0].Client

	// Unset the environment variable so that agent picks up the right test
	// cluster address
	defer os.Setenv(api.EnvVaultAddress, os.Getenv(api.EnvVaultAddress))
	os.Unsetenv(api.EnvVaultAddress)

	// Create temp dir for this test run
	tmpDir, err := os.MkdirTemp("", "TestAutoAuth_SelfHealing")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Write a policy with correct access to the secrets
	serverClient.Sys().PutPolicy("kv-access", kvAccessPolicy)

	// Create a token without enough policy access to the kv secrets
	secret, err := serverClient.Auth().Token().Create(&api.TokenCreateRequest{})
	require.NoError(t, err)
	token := secret.Auth.ClientToken

	// Write token to vault-token file
	tokenFilePath := filepath.Join(tmpDir, "vault-token")
	tokenFile, err := os.Create(tokenFilePath)
	require.NoError(t, err)
	_, err = tokenFile.WriteString(token)
	require.NoError(t, err)
	err = tokenFile.Close()
	require.NoError(t, err)

	defer os.Remove(tokenFilePath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	// Create auth handler
	am, err := tokenfile.NewTokenFileAuthMethod(&auth.AuthConfig{
		Logger: logger.Named("auth.method"),
		Config: map[string]interface{}{
			"token_file_path": filepath.Join(filepath.Join(tmpDir, "vault-token")),
		},
	})
	require.NoError(t, err)
	ahConfig := &auth.AuthHandlerConfig{
		Logger:                       logger.Named("auth.handler"),
		Client:                       serverClient,
		EnableExecTokenCh:            true,
		EnableReauthOnNewCredentials: true,
		ExitOnError:                  false,
	}
	ah := auth.NewAuthHandler(ahConfig)
	errCh := make(chan error)

	go func() {
		errCh <- ah.Run(ctx, am)
	}()
	defer func() {
		select {
		case <-ctx.Done():
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
		}
	}()

	// Create sink file server
	sinkFilePath := filepath.Join(tmpDir, "token-file")
	_, err = os.Create(sinkFilePath)
	defer os.Remove(sinkFilePath)
	require.NoError(t, err)

	config := &sink.SinkConfig{
		Logger: logger.Named("sink.file"),
		Config: map[string]interface{}{
			"path": sinkFilePath,
		},
	}

	fs, err := file.NewFileSink(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Sink = fs

	ss := sink.NewSinkServer(&sink.SinkServerConfig{
		Logger: logger.Named("sink.server"),
		Client: serverClient,
	})

	go func() {
		errCh <- ss.Run(ctx, ah.OutputCh, []*sink.SinkConfig{config}, ah.AuthInProgress)
	}()
	defer func() {
		select {
		case <-ctx.Done():
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
		}
	}()

	// Create template server
	sc := template.ServerConfig{
		Logger: logging.NewVaultLogger(hclog.Trace),
		AgentConfig: &agentConfig.Config{
			Vault: &agentConfig.Vault{
				Address:       serverClient.Address(),
				TLSSkipVerify: true,
			},
			TemplateConfig: &agentConfig.TemplateConfig{
				StaticSecretRenderInt: time.Second * 5,
			},
			// Need to crate at least one sink output so that it does not exit after rendering
			AutoAuth: &agentConfig.AutoAuth{
				Sinks: []*agentConfig.Sink{
					{
						Type: "file",
						Config: map[string]interface{}{
							"path": filepath.Join(filepath.Join(tmpDir, "kvData")),
						},
					},
				},
			},
			ExitAfterAuth: false,
		},
		LogLevel:      hclog.Trace,
		LogWriter:     hclog.DefaultOutput,
		ExitAfterAuth: false,
	}

	templateTest := &ctconfig.TemplateConfig{
		Contents: pointerutil.StringPtr(kvDataTemplateContents),
	}
	dstFile := fmt.Sprintf("%s/%s", tmpDir, "kvData")
	templateTest.Destination = pointerutil.StringPtr(dstFile)
	templatesToRender := []*ctconfig.TemplateConfig{templateTest}

	var server *template.Server
	server = template.NewServer(&sc)

	go func() {
		errCh <- server.Run(ctx, ah.TemplateTokenCh, templatesToRender, ah.AuthInProgress, ah.InvalidToken)
	}()
	defer func() {
		select {
		case <-ctx.Done():
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
		}
	}()

	// Must be done at the very end so that nothing is blocking
	defer cancel()

	// Trigger template render
	ah.TemplateTokenCh <- token
	_, err = waitForFiles(t, filepath.Join(tmpDir, "token-file"), time.Time{})
	require.NoError(t, err)

	tokenInSink, err := os.ReadFile(filepath.Join(tmpDir, "token-file"))
	require.NoError(t, err)
	require.Equal(t, string(tokenInSink), token)

	// Create new token with the correct policy access
	tokenSecret, err := serverClient.Auth().Token().Create(&api.TokenCreateRequest{
		Policies: []string{"kv-access"},
	})
	require.NoError(t, err)
	newToken := tokenSecret.Auth.ClientToken

	// Write token to file
	err = os.WriteFile(filepath.Join(tmpDir, "vault-token"), []byte(token), 0o600)
	require.NoError(t, err)

	// Wait for any potential *incorrect* re-triggers of auto auth
	time.Sleep(time.Second * 5)

	// Auto auth should not have been re-triggered because of just a permission denied error
	// Verify that the new token has NOT been written to the token sink
	tokenInSink, err = os.ReadFile(filepath.Join(tmpDir, "token-file"))
	require.NoError(t, err)
	require.NotEqual(t, string(tokenInSink), newToken)
	require.Equal(t, string(tokenInSink), token)
}

func waitForFiles(t *testing.T, filePath string, prevModTime time.Time) (os.FileInfo, error) {
	t.Helper()

	var err error
	var fileInfo os.FileInfo
	tick := time.Tick(100 * time.Millisecond)
	timeout := time.After(5 * time.Second)
	// We need to wait for the templates to render...
	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("timed out waiting for templates to render, last error: %w", err)
		case <-tick:
		}

		fileInfo, err = os.Stat(filePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		// Keep waiting until the file has been updated since the previous mod time
		if !fileInfo.ModTime().After(prevModTime) {
			continue
		}

		return fileInfo, nil
	}
}
