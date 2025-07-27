#!/bin/bash
# Setup pre-commit hooks for ObjectFS development
# This script installs and configures pre-commit hooks for consistent code quality

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print colored output
print_status() {
    echo -e "${BLUE}🔧 ObjectFS Hook Setup:${NC} $1"
}

print_success() {
    echo -e "${GREEN}✅${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}⚠️${NC} $1"
}

print_error() {
    echo -e "${RED}❌${NC} $1"
}

# Check if we're in a git repository
if [ ! -d ".git" ]; then
    print_error "Not in a git repository. Please run this script from the ObjectFS root directory."
    exit 1
fi

print_status "Setting up ObjectFS development hooks..."

# Check if pre-commit is installed
if ! command -v pre-commit &> /dev/null; then
    print_warning "pre-commit not found. Installing..."
    
    # Try to install pre-commit using different package managers
    if command -v pip3 &> /dev/null; then
        pip3 install pre-commit
    elif command -v pip &> /dev/null; then
        pip install pre-commit
    elif command -v brew &> /dev/null; then
        brew install pre-commit
    elif command -v apt-get &> /dev/null; then
        sudo apt-get update && sudo apt-get install -y pre-commit
    else
        print_error "Could not install pre-commit. Please install it manually:"
        print_error "  pip install pre-commit"
        print_error "  # or"
        print_error "  brew install pre-commit"
        exit 1
    fi
fi

print_success "pre-commit is available"

# Check if golangci-lint is installed
if ! command -v golangci-lint &> /dev/null; then
    print_warning "golangci-lint not found. Installing..."
    
    if command -v brew &> /dev/null; then
        brew install golangci-lint
    else
        # Install using curl
        curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v1.55.2
        
        # Add to PATH if not already there
        export PATH=$PATH:$(go env GOPATH)/bin
        echo 'export PATH=$PATH:$(go env GOPATH)/bin' >> ~/.bashrc 2>/dev/null || true
        echo 'export PATH=$PATH:$(go env GOPATH)/bin' >> ~/.zshrc 2>/dev/null || true
    fi
fi

print_success "golangci-lint is available"

# Check if gosec is installed
if ! command -v gosec &> /dev/null; then
    print_warning "gosec not found. Installing..."
    go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest
fi

print_success "gosec is available"

# Install pre-commit hooks
print_status "Installing pre-commit hooks..."
pre-commit install

print_success "Pre-commit hooks installed"

# Install commit-msg hook for conventional commits (optional)
print_status "Installing commit-msg hook..."
pre-commit install --hook-type commit-msg || print_warning "Could not install commit-msg hook (optional)"

# Create golangci-lint configuration if it doesn't exist
if [ ! -f ".golangci.yml" ]; then
    print_status "Creating golangci-lint configuration..."
    cat > .golangci.yml << 'EOF'
# ObjectFS golangci-lint configuration
# Ensures consistent code quality and style

run:
  timeout: 5m
  tests: true
  skip-dirs:
    - vendor
    - testdata
  skip-files:
    - ".*\\.pb\\.go$"
    - ".*_test\\.go$" # Skip some test files if needed

output:
  format: colored-line-number
  print-issued-lines: true
  print-linter-name: true

linters-settings:
  gocyclo:
    min-complexity: 15
  goconst:
    min-len: 2
    min-occurrences: 2
  gofmt:
    simplify: true
  goimports:
    local-prefixes: github.com/objectfs/objectfs
  golint:
    min-confidence: 0
  govet:
    check-shadowing: true
  misspell:
    locale: US
  unused:
    check-exported: false

linters:
  enable:
    - bodyclose
    - deadcode
    - depguard
    - dogsled
    - dupl
    - errcheck
    - gochecknoinits
    - goconst
    - gocyclo
    - gofmt
    - goimports
    - golint
    - gosec
    - gosimple
    - govet
    - ineffassign
    - interfacer
    - misspell
    - nakedret
    - prealloc
    - staticcheck
    - structcheck
    - typecheck
    - unconvert
    - unparam
    - unused
    - varcheck
    - whitespace

issues:
  exclude-rules:
    # Exclude some linters from running on tests files.
    - path: _test\.go
      linters:
        - gocyclo
        - errcheck
        - dupl
        - gosec
    # Exclude known linter issues
    - text: "weak cryptographic primitive"
      linters:
        - gosec
    # Exclude shadow checks for err variables
    - text: 'shadow: declaration of "err"'
      linters:
        - govet
EOF
    print_success "Created .golangci.yml configuration"
fi

# Run pre-commit on all files to test
print_status "Running pre-commit on all files (this may take a moment)..."
if pre-commit run --all-files; then
    print_success "All pre-commit hooks passed!"
else
    print_warning "Some pre-commit hooks failed. This is normal for the first run."
    print_warning "Fix any issues and commit again, or run 'pre-commit run --all-files' to check."
fi

# Create a simple Git hook for additional ObjectFS-specific checks
print_status "Creating ObjectFS-specific pre-commit hook..."
cat > .git/hooks/pre-commit << 'EOF'
#!/bin/bash
# ObjectFS custom pre-commit hook
# Additional checks specific to ObjectFS development

# Check for debugging statements
if git diff --cached --name-only | grep -E '\.(go)$' | xargs grep -n "fmt\.Print\|log\.Print\|TODO\|FIXME" 2>/dev/null; then
    echo "❌ Found debugging statements or TODO/FIXME comments in staged files"
    echo "Please remove them before committing"
    exit 1
fi

# Check that version is updated if main.go changed
if git diff --cached --name-only | grep -q "cmd/objectfs/main.go"; then
    if ! git diff --cached | grep -q "Version.*="; then
        echo "⚠️  main.go changed but version string not updated"
        echo "Consider updating the version if this is a release"
    fi
fi

# Run the standard pre-commit hooks
exec pre-commit hook-impl --config=.pre-commit-config.yaml --hook-type=pre-commit --hook-dir .git/hooks --color=always "$@"
EOF

chmod +x .git/hooks/pre-commit
print_success "Created ObjectFS-specific pre-commit hook"

print_success "🎉 ObjectFS development hooks setup complete!"
print_status "
Next steps:
  1. All commits will now run pre-commit hooks automatically
  2. To run hooks manually: pre-commit run --all-files  
  3. To update hooks: pre-commit autoupdate
  4. To skip hooks (not recommended): git commit --no-verify

Hook configuration:
  📋 Code formatting (gofmt, goimports)
  🔍 Linting (golangci-lint, gosec)
  🧪 Testing (go test with race detection)
  📊 Coverage (minimum 80% required)
  🔒 Security scanning (gosec)
  📚 Documentation (markdown linting)
  
Happy coding! 🚀"