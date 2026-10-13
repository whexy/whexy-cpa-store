package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestManagementResources(t *testing.T) {
	raw, errRegister := handleMethod(pluginabi.MethodManagementRegister, []byte(`{}`))
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	var envelope struct {
		OK     bool                                     `json:"ok"`
		Result pluginapi.ManagementRegistrationResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil || !envelope.OK {
		t.Fatalf("registration %s: %v", raw, errUnmarshal)
	}
	resources := envelope.Result.Resources
	if len(resources) != 2 || resources[0].Path != "/v0/resource/plugins/usage-dashboard/index.html" || resources[0].Menu != "Usage" ||
		resources[1].Path != "/v0/resource/plugins/usage-dashboard/logo.svg" || resources[1].Menu != "" {
		t.Fatalf("resources = %+v", resources)
	}
}

func TestLogo(t *testing.T) {
	resp := routeManagement(pluginapi.ManagementRequest{Method: "get", Path: logoPath})
	if resp.StatusCode != http.StatusOK || !bytes.Equal(resp.Body, logoSVG) || resp.Headers.Get("Content-Type") != "image/svg+xml" || resp.Headers.Get("Content-Security-Policy") != "default-src 'none'" {
		t.Fatalf("logo = %d %v", resp.StatusCode, resp.Headers)
	}
	var svg struct {
		XMLName xml.Name
		ViewBox string `xml:"viewBox,attr"`
	}
	if errParse := xml.Unmarshal(logoSVG, &svg); errParse != nil || svg.XMLName.Local != "svg" || svg.ViewBox != "0 0 24 24" {
		t.Fatalf("logo is not a 24x24 SVG: %+v %v", svg, errParse)
	}
	if pluginRegistration().Metadata.Logo != logoPath {
		t.Fatalf("Metadata.Logo = %q", pluginRegistration().Metadata.Logo)
	}
}
