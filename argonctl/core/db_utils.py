\
import os

def get_core_db_path(db_name_env_var="DB_NAME", default_db_name="argon.db"):
    """
    Determines the path for the core Argon database file.
    This is typically a central database for Argon, not project-specific metadata.
    """
    argon_user_dir = os.path.expanduser("~/.argon")
    os.makedirs(argon_user_dir, exist_ok=True) 
    db_name = os.getenv(db_name_env_var, default_db_name)
    return os.path.join(argon_user_dir, db_name)

def get_project_db_path(project_name, default_db_name="metadata.db"):
    """
    Determines the path for a project-specific metadata database file.
    """
    if not project_name:
        raise ValueError("Project name cannot be empty when getting project DB path.")
    project_argon_dir = os.path.join(os.path.expanduser("~/.argon"), project_name)
    os.makedirs(project_argon_dir, exist_ok=True)
    return os.path.join(project_argon_dir, default_db_name)
