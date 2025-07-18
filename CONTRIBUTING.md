# Contributing to Argon

Thank you for your interest in contributing to Argon! This document provides guidelines for contributors.

## 🤝 How to Contribute

### 1. Fork and Clone
```bash
git clone https://github.com/your-username/argon.git
cd argon
```

### 2. Development Setup
```bash
# Start services
docker compose up -d

# Install dependencies
cd cli && go mod tidy
cd ../services/api && pip install -r requirements.txt
```

### 3. Make Changes
- **Go engine**: `services/engine/`
- **Python API**: `services/api/`  
- **CLI tool**: `cli/`

### 4. Test Your Changes
```bash
./test_complete_system.sh
```

### 5. Submit Pull Request
```bash
git checkout -b feature/your-feature
git commit -m "Clear description of changes"
git push origin feature/your-feature
```

## 📋 Guidelines

### Code Quality
- Follow Go and Python style guides
- Include tests for new features
- Ensure all tests pass before submitting
- Update documentation for changes

### Commit Guidelines
- Use clear, descriptive commit messages
- Keep commits focused and atomic
- Reference issue numbers when applicable

## 🚫 What NOT to Include

### AI Assistant Content
**Do not include AI assistant-related content in commits:**
- No `CLAUDE.md`, `claude.md`, or AI conversation files
- No AI-generated signatures in code comments
- No references to AI assistants in commit messages or code
- No `.claude/` directories or AI workspace files

The `.gitignore` file excludes these automatically.

### Other Exclusions
- No personal credentials or API keys
- No IDE-specific configuration files
- No compiled binaries or cache files

## 🧪 Testing

### Run Tests
```bash
# Storage engine tests
cd services/engine && go test ./...

# Full integration test
./test_complete_system.sh
```

### Manual Testing
```bash
# Test CLI
cd cli && go build -o argon . && ./argon --help

# Test API
curl http://localhost:3000/health
```

## 🐛 Bug Reports

Include:
- Clear description of the issue
- Steps to reproduce
- Expected vs actual behavior
- Environment details (OS, versions)
- Relevant logs or errors

## 💡 Feature Requests

- Check existing issues first
- Describe the use case clearly
- Explain user benefits
- Provide implementation ideas

## 🔄 Development Workflow

### Branch Naming
- `feature/description` - New features
- `fix/description` - Bug fixes
- `docs/description` - Documentation
- `refactor/description` - Refactoring

### Review Process
1. Create clear pull request
2. Ensure all tests pass
3. Address review feedback
4. Merge after approval

## 📞 Support

- **Issues**: Bug reports and technical questions
- **Discussions**: General questions and ideas
- **Documentation**: Check `docs/` folder

## 🏆 Recognition

Contributors are credited in README and release notes.

## 📄 License

By contributing, you agree your contributions will be licensed under the MIT License.

---

Thank you for helping make Argon better! 🚀
