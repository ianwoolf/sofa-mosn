/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mtls

import (
	"sync"
	"sync/atomic"

	"sofastack.io/sofa-mosn/pkg/api/v2"
	"sofastack.io/sofa-mosn/pkg/log"
	"sofastack.io/sofa-mosn/pkg/mtls/crypto/tls"
	"sofastack.io/sofa-mosn/pkg/types"
)

var (
	secretManagerInstance = &secretManager{
		validations: make(map[string]*validation),
	}
	sdsCallbacks = []func(){}
)

func RegisterSdsCallback(f func()) {
	sdsCallbacks = append(sdsCallbacks, f)
}

type validation struct {
	// pem stored the validation pem string
	pem string
	// certificates stored the certificates that are signed by the validation
	certificates map[string]*sdsProvider
}

type secretManager struct {
	mutex       sync.Mutex
	validations map[string]*validation
}

func getOrCreateProvider(cfg *v2.TLSConfig) *sdsProvider {
	return secretManagerInstance.getOrCreateProvider(cfg)
}

func ClearSecretManager() {
	secretManagerInstance.mutex.Lock()
	defer secretManagerInstance.mutex.Unlock()
	secretManagerInstance.validations = make(map[string]*validation)
	log.DefaultLogger.Infof("[mtls] [sds provider] clear all providers")
}

func (mng *secretManager) getOrCreateProvider(cfg *v2.TLSConfig) *sdsProvider {
	mng.mutex.Lock()
	defer mng.mutex.Unlock()
	validationName := cfg.SdsConfig.ValidationConfig.Name
	v, ok := mng.validations[validationName]
	if !ok {
		// add a validation
		v = &validation{
			certificates: make(map[string]*sdsProvider),
		}
		mng.validations[validationName] = v
	}
	certName := cfg.SdsConfig.CertificateConfig.Name
	p, ok := v.certificates[certName]
	if !ok {
		// new a provider
		p = &sdsProvider{
			config: cfg,
			info: &secretInfo{
				Validation: v.pem,
			},
		}
		v.certificates[certName] = p
		// set a certificate callback
		client := GetSdsClient(cfg.SdsConfig.CertificateConfig)
		client.AddUpdateCallback(cfg.SdsConfig.CertificateConfig, p.setCertificate)
		// set a validation callback
		client.AddUpdateCallback(cfg.SdsConfig.ValidationConfig, mng.setValidation)
		log.DefaultLogger.Infof("[mtls] [sds provider] add a new sds provider %s", certName)
	}

	return p
}

// setValidation is called in sds client
func (mng *secretManager) setValidation(name string, secret *types.SdsSecret) {
	mng.mutex.Lock()
	defer mng.mutex.Unlock()
	v, ok := mng.validations[name]
	if !ok {
		return
	}
	if secret.ValidationPEM != "" {
		v.pem = secret.ValidationPEM
		log.DefaultLogger.Infof("[mtls] [sds provider] provider %s receive a validation set", name)
		// set the validation
		for _, cert := range v.certificates {
			cert.setValidation(v.pem)
		}
	}
}

// sdsProvider is an implementation of types.Provider
// sdsProvider stored a tls context that makes by sds
// do not support delete certificate for sds api
type sdsProvider struct {
	value  atomic.Value // stored tlsContext
	config *v2.TLSConfig
	info   *secretInfo
}

func (p *sdsProvider) setValidation(v string) {
	p.info.Validation = v
	if p.info.full() {
		p.update()
	}
}

func (p *sdsProvider) setCertificate(name string, secret *types.SdsSecret) {
	if secret.CertificatePEM != "" {
		p.info.Certificate = secret.CertificatePEM
		p.info.PrivateKey = secret.PrivateKeyPEM
		log.DefaultLogger.Infof("[mtls] [sds provider] provider %s receive a cerificate set", name)
	}
	if p.info.full() {
		p.update()
	}
}

func (p *sdsProvider) update() {
	ctx, err := newTLSContext(p.config, p.info)
	if err != nil {
		log.DefaultLogger.Errorf("[mtls] [sds] update tls context failed: %v", err)
		return
	}
	p.value.Store(ctx)
	log.DefaultLogger.Infof("[mtls] [sds] update tls context success")
	// notify certificates updates
	for _, cb := range sdsCallbacks {
		cb()
	}
}

func (p *sdsProvider) GetTLSConfig(client bool) *tls.Config {
	v := p.value.Load()
	ctx, ok := v.(*tlsContext)
	if !ok {
		return nil
	}
	return ctx.GetTLSConfig(client)
}

func (p *sdsProvider) MatchedServerName(sn string) bool {
	v := p.value.Load()
	ctx, ok := v.(*tlsContext)
	if !ok {
		return false
	}
	return ctx.MatchedServerName(sn)
}

func (p *sdsProvider) MatchedALPN(protos []string) bool {
	v := p.value.Load()
	ctx, ok := v.(*tlsContext)
	if !ok {
		return false
	}
	return ctx.MatchedALPN(protos)
}

func (p *sdsProvider) Ready() bool {
	v := p.value.Load()
	_, ok := v.(*tlsContext)
	return ok
}

func (p *sdsProvider) Empty() bool {
	return false
}
