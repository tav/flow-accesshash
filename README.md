# Usage
### Installation
```
go mod tidy
```

If you encounter an issue such as:
```
fatal error: 'relic.h' file not found
#include "relic.h"
         ^~~~~~~~~
```

Build the appropriate version of flow-go/crypto according to the version specified in the go.mod such as:
```
cd $GOPATH/pkg/mod/github.com/onflow/flow-go/crypto@v0.25.0/
go generate
```

You can validate @
```
go run -tags relic ./cmd/verify-access-api/verify-access-api.go access-001.canary1.nodes.onflow.org:9000
```


or with a specified BlockHeight
```
go run -tags relic ./cmd/verify-access-api/verify-access-api.go access-001.canary1.nodes.onflow.org:9000 2604333
```