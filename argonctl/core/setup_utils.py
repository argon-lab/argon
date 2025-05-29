"""
First-time setup utilities for argonctl.
Handles environment variable configuration and validation.
"""
import os
import sys
from pathlib import Path
from typing import Dict, List, Optional, Tuple

try:
    from dotenv import load_dotenv, set_key
except ImportError:
    print("[WARN] python-dotenv not installed, some features may not work correctly")

# ANSI color codes for CLI output
COLOR_INFO = "\033[32m[INFO]\033[0m"
COLOR_WARN = "\033[33m[WARN]\033[0m"
COLOR_ERROR = "\033[31m[ERROR]\033[0m"

REQUIRED_ENV_VARS = {
    'AWS_ACCESS_KEY_ID': 'Your AWS access key ID',
    'AWS_SECRET_ACCESS_KEY': 'Your AWS secret access key',
    'S3_BUCKET': 'S3 bucket name for Argon to use'
}

OPTIONAL_ENV_VARS = {
    'AWS_REGION': ('AWS region (e.g., us-east-1)', 'us-east-1'),
    'ARGON_BASE_SNAPSHOT_S3_PATH': ('S3 path for base snapshot', 'base/dump.archive'),
    'ARGON_AUTO_SUSPEND_ENABLED': ('Enable dashboard auto-suspend', 'true'),
    'DASHBOARD_AUTO_SUSPEND_IDLE_MINUTES': ('Minutes before auto-suspend', '60')
}

def find_dotenv() -> Tuple[Optional[str], Optional[str]]:
    """
    Find .env file in the current directory or user's home directory.
    Returns tuple of (cwd_env_path, user_env_path).
    """
    cwd_env = os.path.join(os.getcwd(), '.env')
    home = os.path.expanduser('~')
    user_env = os.path.join(home, '.argon', '.env')
    
    return cwd_env if os.path.exists(cwd_env) else None, user_env if os.path.exists(user_env) else None

def load_environment() -> Dict[str, str]:
    """
    Load environment variables from .env files.
    Returns dict of loaded variables.
    """
    cwd_env, user_env = find_dotenv()
    loaded_vars = {}
    
    if cwd_env:
        load_dotenv(cwd_env)
        print(f"{COLOR_INFO} Loaded environment from {cwd_env}")
        loaded_vars['env_file'] = cwd_env
    elif user_env:
        load_dotenv(user_env)
        print(f"{COLOR_INFO} Loaded environment from {user_env}")
        loaded_vars['env_file'] = user_env
    
    # Collect loaded env vars
    for var in {**REQUIRED_ENV_VARS, **OPTIONAL_ENV_VARS}:
        if os.getenv(var):
            loaded_vars[var] = os.getenv(var)
    
    return loaded_vars

def create_env_file(path: str) -> None:
    """Create a new .env file and its parent directories."""
    os.makedirs(os.path.dirname(path), exist_ok=True)
    if not os.path.exists(path):
        with open(path, 'w') as f:
            f.write("# Argon CLI Configuration\n\n")
            print(f"{COLOR_INFO} Created new .env file at {path}")

def prompt_missing_env_vars(loaded_vars: Dict[str, str], env_file: str) -> None:
    """
    Interactively prompt for missing required environment variables
    and optionally configure optional ones.
    """
    print(f"\n{COLOR_INFO} Welcome to argonctl first-time setup!")
    print("This wizard will help you configure the required environment variables.\n")
    
    # First handle required vars
    for var, description in REQUIRED_ENV_VARS.items():
        if var not in loaded_vars:
            value = input(f"{var} ({description}): ").strip()
            if value:
                set_key(env_file, var, value)
                os.environ[var] = value
                loaded_vars[var] = value
            else:
                print(f"{COLOR_WARN} {var} is required but was not set")
    
    # Then handle optional vars
    print("\nOptional Configuration (press Enter to skip/use defaults):")
    for var, (description, default) in OPTIONAL_ENV_VARS.items():
        if var not in loaded_vars:
            value = input(f"{var} ({description}) [{default}]: ").strip()
            value = value if value else default
            set_key(env_file, var, value)
            os.environ[var] = value
            loaded_vars[var] = value

def check_environment(skip_aws_validation: bool = False) -> bool:
    """
    Check if all required environment variables are set.
    Returns True if all required vars are set, False otherwise.
    """
    loaded_vars = load_environment()
    
    # If no .env file exists, create one in the user's home directory
    if 'env_file' not in loaded_vars:
        home = os.path.expanduser('~')
        env_file = os.path.join(home, '.argon', '.env')
        create_env_file(env_file)
        loaded_vars['env_file'] = env_file
    
    missing_vars = [var for var in REQUIRED_ENV_VARS if var not in loaded_vars]
    
    if missing_vars or not skip_aws_validation:
        prompt_missing_env_vars(loaded_vars, loaded_vars['env_file'])
        
        # Recheck after prompting
        still_missing = [var for var in REQUIRED_ENV_VARS if not os.getenv(var)]
        if still_missing:
            print(f"{COLOR_ERROR} The following required environment variables are still missing:")
            for var in still_missing:
                print(f"  - {var} ({REQUIRED_ENV_VARS[var]})")
            return False
    
    print(f"{COLOR_INFO} Environment configuration is complete!")
    return True
