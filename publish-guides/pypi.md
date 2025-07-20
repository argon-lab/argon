# Publishing Python SDK to PyPI

## Prerequisites
- PyPI account (create at https://pypi.org)
- Test PyPI account (create at https://test.pypi.org)
- Install build tools: `pip install build twine`

## Steps to Publish

### 1. Prepare the Package
```bash
# Ensure you're in the argon directory
cd /path/to/argon

# Clean previous builds
rm -rf dist/ build/ *.egg-info
```

### 2. Build the Package
```bash
# Build both wheel and source distribution
python -m build

# This creates:
# dist/argon_mongodb-1.0.0-py3-none-any.whl
# dist/argon_mongodb-1.0.0.tar.gz
```

### 3. Test on TestPyPI First
```bash
# Upload to TestPyPI
python -m twine upload --repository testpypi dist/*

# Test installation from TestPyPI
pip install --index-url https://test.pypi.org/simple/ --extra-index-url https://pypi.org/simple/ argon-mongodb

# Test the package
python -c "from argon import ArgonClient; print('Success!')"
```

### 4. Publish to PyPI
```bash
# Upload to real PyPI
python -m twine upload dist/*

# Enter your PyPI credentials
# Username: __token__
# Password: pypi-AgEIcHlwaS5vcmc... (your API token)
```

### 5. Users Install
```bash
# Basic installation
pip install argon-mongodb

# With ML integrations
pip install argon-mongodb[ml]
```

## API Token Setup (Recommended)
1. Go to https://pypi.org/manage/account/token/
2. Create new API token
3. Save token securely
4. Use with twine:
   ```bash
   # Create ~/.pypirc
   [pypi]
   username = __token__
   password = pypi-AgEIcHlwaS5vcmc...
   ```

## Version Management
- Update version in pyproject.toml
- Follow semantic versioning
- Tag releases in git: `git tag v1.0.0`

## Automation with GitHub Actions
Consider adding `.github/workflows/publish-python.yml` for automated releases.