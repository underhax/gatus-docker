.PHONY: check test coverage vulncheck verify security-trivy docker-lint \
        secrets-scan clean

check:
	@test -z "$$(gofmt -s -l .)" || (echo "Unformatted files found. Run 'gofmt -s -w .' to fix them." && false)
	golangci-lint run ./...
	go build -o /dev/null ./init
	go build -o /dev/null ./probe

test:
	go test -v -race ./...

coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

vulncheck:
	govulncheck ./...

docker-lint:
	hadolint Dockerfile

security-trivy:
	trivy fs --severity CRITICAL,HIGH .

secrets-scan:
	gitleaks detect --source . --no-git --no-color

verify: check test vulncheck docker-lint security-trivy secrets-scan

clean:
	rm -rf coverage coverage.out
