// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

//go:build windows

package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elastic/elastic-agent-libs/logp"
)

func TestGetServiceStates(t *testing.T) {
	handle, err := openSCManager("", "", ScManagerEnumerateService|ScManagerConnect)
	assert.NoError(t, err)
	assert.NotEqual(t, handle, InvalidDatabaseHandle)
	services, err := GetServiceStates(logp.L(), handle, ServiceStateAll, map[string]struct{}{})
	assert.NoError(t, err)
	assert.True(t, len(services) > 0)
	closeHandle(handle)
}
