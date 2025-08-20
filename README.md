# Provider SSH

## Overview

The `provider-ssh` is a Crossplane provider that reconciles external resources reachable over SSH. It executes user-supplied scripts to converge a remote system to a desired state, exposes observed state from checks, and supports idempotency and drift detection.

### Kinds

- `ProviderConfig`: to authenticate/reach the target(s)
- `SSHTask`: A unit of desired work on a host


## Installation

You can run the `provider-ssh` locally or install it from an xpkg file. To install the provider use:
```yaml
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-ssh
spec:
  package: docker.io/etesami/provider-ssh:latest
```

## ProviderConfig

To begin, you'll need to create a `ProviderConfig` and a `Secret`. 
To initiate a connection to the remote host, either a `password` or `privateKey` is required. 
The `privateKey` should be provided as a single-line, base64-encoded string. 
To generate the base64 version of a private key, use the following command:

```bash
# In Linux
cat .ssh/id_rsa | base64 -w0
```
The `knownHosts` file is required to verify the identity of the server. 
You can generate and verify it using the command below:

```bash
ssh-keyscan <HOST-REMOTE-IP>
```

Next, construct the `ProviderConfig` and `Secret` as shown below:

```yaml
apiVersion: v1
kind: Secret
metadata:
  namespace: crossplane-system
  name: providerssh-secret
type: Opaque
stringData:
  config: |
    {
      "username": "ubuntu",
      "password": "password",
      "privateKey": "5XUUNPV2tSd0ptTFp...wbTNFKzhqMkYzdXc5ClNRZ09QO",
      "hostIP": "10.29.30.5",
      "hostPort": "22",
      "knownHosts": "10.29.30.5 ecdsa-sha2-nistp256 AAAAE2VjZHN...UpvT57WP45MDBAV4CxQ="
    }
---
apiVersion: ssh.crossplane.io/v1alpha1
kind: ProviderConfig
metadata:
  name: providerssh-config
spec:
  credentials:
    source: Secret
    secretRef:
      namespace: crossplane-system
      name: providerssh-secret
      key: config
```

## SSHTask

```yaml
apiVersion: ssh.crossplane.io/v1alpha1
kind: SSHTask
metadata:
  name: configure-nginx
spec:
  providerConfigRef:
    name: default
  managementPolicies: ["*"]
  forProvider:
    scripts:

      # 1) PROBE: collect facts + compliance + optional drift (JSON)
      #    Example of the output should be like this:
      #    {
      #      "facts": {... arbitrary JSON ...},
      #      "compliant": true,
      #      "drift": {... optional ...}
      #    }
      # Required
      probeScript:
        inline: |
          set -euo pipefail
          compliant=true
          if ! command -v nginx >/dev/null 2>&1; then
            compliant=false
          fi
          version=""
          if $compliant; then
            version=$(nginx -v 2>&1 | cut -d'/' -f2)
          fi
          jq -n --argjson c $compliant --arg v "$version" \
            '{facts:{nginx:{version:$v}},compliant:$c}'

      # Ensure: install nginx
      # Required
      ensureScript:
        inline: |
          set -euo pipefail
          sudo apt-get update -y
          sudo apt-get install -y nginx jq

      # Cleanup: remove nginx (Optional)
      cleanupScript:
        inline: |
          sudo apt-get purge -y nginx || true

    observe:
      refreshPolicy: Always          # Always | IfStale
      freshnessTTL: 60s              # ignored with Always
      capture: stdout                # stdout | stderr | both | none
      map:
        - from: .nginx.version
          to: version

    # Execution environment & safety
    execution:
      sudo: true
      shell: /bin/bash -euo pipefail
      timeoutSeconds: 600
      maxAttempts: 2              # per reconcile
      # env is map of key, values where all instance of key is replaced
      # by value before executing the script on the remove device
      env:
        INLINE_VAR: FOO

    artifactPolicy:
      capture: stdout        # stdout | stderr | both | none
        
      
status:
  conditions:
    - type: Ready
      status: "True"
      reason: UpToDate
      lastTransitionTime: "2025-08-18T14:02:31Z"
    - type: Synced
      status: "True"
      reason: ObserveSucceeded
      lastTransitionTime: "2025-08-18T14:02:31Z"
  atProvider:
    endpoint:
      host: "10.0.0.12"
      port: 22
      username: "ubuntu"
    lastCheckTime: "2025-08-18T14:02:29Z"
    lastRun:
      time: "2025-08-18T14:02:29Z"
      exitCode: 0
      retryCount: 0
    artifacts:
      stdout: ""
      stderr: ""
    observed:
      raw: {}                # the full JSON payload (optional, gated by size limit)
      fields:                # mapped key/value facts
        version: "1.24.0"
        indexETag: "d41d8cd98f00b204e9800998ecf8427e"
      digest: "sha256:..."   # hash of observe payload for cheap change detection
      observedAt: "..."      # RFC3339
    
```

## Controller Logic


### Observe
- If no `probeScript` → Resource is not up to date, requeue for update  
- If resource is current (not stale) → mark as **Ready**  
- Run `probeScript`, save artifact  
  - If failed → requeue for update  
  - If successful → parse output (digest + field mapping)  
    - If compliant → mark as **Ready**  
    - If not compliant → requeue for update  

### Update
- If no `ensureScript` → reconcile error  
- Run `ensureScript`, save artifact  
  - If failed → reconcile error, mark as **Not Ready**  
  - If successful → run `probeScript` again  
    - If failed → **Ready = false**, requeue  
    - If successful → process output  
      - Save to `status.observed`  
      - If compliant → **Done**  
      - If not compliant → return empty  


## Development

```bash
make build
# The image is stored somewhere like
pkg=provider-ssh-v0.0.0-21.gb69de81.xpkg
VERSION=v2.0.0-rc6 && \
  DIR=/home/ubuntu/provider-ssh/_output/xpkg/linux_amd64 && \
  crossplane xpkg push -f $DIR/$pkg index.docker.io/etesami/provider-ssh:$VERSION && \
  crossplane xpkg push -f $DIR/$pkg index.docker.io/etesami/provider-ssh:latest
```