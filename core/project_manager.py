class ProjectManager:
    def __init__(self):
        print("Initializing Dummy ProjectManager")

    def list_projects(self):
        print("Dummy ProjectManager: Listing projects")
        return []

    def create_project(self, name):
        print(f"Dummy ProjectManager: Creating project {name}")
        return {"name": name, "status": "created"}

    def delete_project(self, name):
        print(f"Dummy ProjectManager: Deleting project {name}")
        return True
