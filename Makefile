.PHONY: ui build test tidy

# go:embed cannot reach outside backend/, so the UI is copied into the embed dir
# before every build and test. The delete keeps a file removed from ui/ from
# staying embedded; .gitkeep is tracked so a fresh clone still compiles.
ui:
	find backend/web/dist -mindepth 1 ! -name .gitkeep -delete
	cp -R ui/. backend/web/dist/

build: ui
	cd backend && go build -o ../relume-tv ./cmd/relume-tv

test: ui
	cd backend && go test ./...

tidy:
	cd backend && go mod tidy
