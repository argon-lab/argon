#!/usr/bin/env python3
"""
Argon v2 Python API Service
Bridges the CLI and Go engine, provides REST API for web dashboard
"""

import os
import asyncio
import logging
from datetime import datetime
from typing import List, Optional, Dict, Any
import subprocess
import json

from fastapi import FastAPI, HTTPException, Depends, status
from fastapi.middleware.cors import CORSMiddleware
from fastapi.security import HTTPBearer, HTTPAuthorizationCredentials
from pydantic import BaseModel, Field
import httpx

# Configure logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = FastAPI(
    title="Argon v2 API",
    description="MongoDB Branching System - Python API Service",
    version="2.0.0",
    docs_url="/docs",
    redoc_url="/redoc"
)

# CORS middleware
app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],  # Configure properly for production
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Security
security = HTTPBearer()

# Configuration
GO_ENGINE_HOST = os.getenv("GO_ENGINE_HOST", "localhost")
GO_ENGINE_PORT = os.getenv("GO_ENGINE_PORT", "8080")
GO_ENGINE_URL = f"http://{GO_ENGINE_HOST}:{GO_ENGINE_PORT}"

# Data models for API
class BranchCreateRequest(BaseModel):
    name: str = Field(..., description="Branch name")
    description: Optional[str] = Field(None, description="Branch description")
    parent_branch: Optional[str] = Field(None, description="Parent branch ID")

class BranchResponse(BaseModel):
    id: str
    name: str
    description: Optional[str]
    status: str
    is_main: bool
    created_at: datetime
    updated_at: datetime
    storage_path: str
    document_count: int
    storage_size: int

class ProjectCreateRequest(BaseModel):
    name: str = Field(..., description="Project name")
    description: Optional[str] = Field(None, description="Project description")
    mongodb_uri: str = Field(..., description="MongoDB connection URI")

class ProjectResponse(BaseModel):
    id: str
    name: str
    description: Optional[str]
    mongodb_uri: str
    status: str
    created_at: datetime
    updated_at: datetime
    branch_count: int
    storage_size: int

class ConnectionStringResponse(BaseModel):
    connection_string: str
    database_name: str
    expires_at: Optional[datetime] = None

class BranchStatsResponse(BaseModel):
    document_count: int
    storage_size: int
    change_count: int
    compression_ratio: float
    last_change_at: Optional[datetime]

# Authentication dependency
async def get_current_user(credentials: HTTPAuthorizationCredentials = Depends(security)):
    """
    Validate API key authentication
    For now, accept any Bearer token - implement proper auth later
    """
    if not credentials.token:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail="Invalid authentication credentials",
            headers={"WWW-Authenticate": "Bearer"},
        )
    return {"user_id": "default_user", "api_key": credentials.token}

# HTTP client for Go engine communication
async def call_go_engine(method: str, endpoint: str, data: Dict[Any, Any] = None) -> Dict[Any, Any]:
    """
    Call the Go engine HTTP API
    """
    url = f"{GO_ENGINE_URL}{endpoint}"
    
    async with httpx.AsyncClient() as client:
        try:
            if method.upper() == "GET":
                response = await client.get(url)
            elif method.upper() == "POST":
                response = await client.post(url, json=data)
            elif method.upper() == "PUT":
                response = await client.put(url, json=data)
            elif method.upper() == "DELETE":
                response = await client.delete(url)
            else:
                raise ValueError(f"Unsupported HTTP method: {method}")
            
            response.raise_for_status()
            return response.json()
        
        except httpx.ConnectError:
            logger.error(f"Failed to connect to Go engine at {url}")
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail="Go engine service unavailable"
            )
        except httpx.HTTPStatusError as e:
            logger.error(f"Go engine returned error: {e.response.status_code} - {e.response.text}")
            raise HTTPException(
                status_code=e.response.status_code,
                detail=f"Go engine error: {e.response.text}"
            )

# Health check
@app.get("/health")
async def health_check():
    """Health check endpoint"""
    try:
        # Check if Go engine is responsive
        response = await call_go_engine("GET", "/health")
        return {
            "status": "healthy",
            "timestamp": datetime.utcnow(),
            "go_engine": "connected",
            "version": "2.0.0"
        }
    except Exception as e:
        return {
            "status": "degraded",
            "timestamp": datetime.utcnow(),
            "go_engine": "disconnected",
            "version": "2.0.0",
            "error": str(e)
        }

# Project endpoints
@app.post("/api/projects", response_model=ProjectResponse)
async def create_project(
    request: ProjectCreateRequest,
    user: dict = Depends(get_current_user)
):
    """Create a new project"""
    data = {
        "name": request.name,
        "description": request.description,
        "mongodb_uri": request.mongodb_uri,
        "user_id": user["user_id"]
    }
    
    result = await call_go_engine("POST", "/api/projects", data)
    return ProjectResponse(**result)

@app.get("/api/projects", response_model=List[ProjectResponse])
async def list_projects(user: dict = Depends(get_current_user)):
    """List all projects for the authenticated user"""
    result = await call_go_engine("GET", f"/api/projects?user_id={user['user_id']}")
    return [ProjectResponse(**project) for project in result["projects"]]

@app.get("/api/projects/{project_id}", response_model=ProjectResponse)
async def get_project(
    project_id: str,
    user: dict = Depends(get_current_user)
):
    """Get project details"""
    result = await call_go_engine("GET", f"/api/projects/{project_id}")
    return ProjectResponse(**result)

@app.delete("/api/projects/{project_id}")
async def delete_project(
    project_id: str,
    user: dict = Depends(get_current_user)
):
    """Delete a project"""
    await call_go_engine("DELETE", f"/api/projects/{project_id}")
    return {"message": "Project deleted successfully"}

# Branch endpoints
@app.post("/api/projects/{project_id}/branches", response_model=BranchResponse)
async def create_branch(
    project_id: str,
    request: BranchCreateRequest,
    user: dict = Depends(get_current_user)
):
    """Create a new branch in a project"""
    data = {
        "project_id": project_id,
        "name": request.name,
        "description": request.description,
        "parent_branch": request.parent_branch,
        "user_id": user["user_id"]
    }
    
    result = await call_go_engine("POST", f"/api/projects/{project_id}/branches", data)
    return BranchResponse(**result)

@app.get("/api/projects/{project_id}/branches", response_model=List[BranchResponse])
async def list_branches(
    project_id: str,
    user: dict = Depends(get_current_user)
):
    """List all branches in a project"""
    result = await call_go_engine("GET", f"/api/projects/{project_id}/branches")
    return [BranchResponse(**branch) for branch in result["branches"]]

@app.get("/api/projects/{project_id}/branches/{branch_id}", response_model=BranchResponse)
async def get_branch(
    project_id: str,
    branch_id: str,
    user: dict = Depends(get_current_user)
):
    """Get branch details"""
    result = await call_go_engine("GET", f"/api/projects/{project_id}/branches/{branch_id}")
    return BranchResponse(**result)

@app.delete("/api/projects/{project_id}/branches/{branch_id}")
async def delete_branch(
    project_id: str,
    branch_id: str,
    user: dict = Depends(get_current_user)
):
    """Delete a branch"""
    await call_go_engine("DELETE", f"/api/projects/{project_id}/branches/{branch_id}")
    return {"message": "Branch deleted successfully"}

@app.get("/api/projects/{project_id}/branches/{branch_id}/stats", response_model=BranchStatsResponse)
async def get_branch_stats(
    project_id: str,
    branch_id: str,
    user: dict = Depends(get_current_user)
):
    """Get branch statistics"""
    result = await call_go_engine("GET", f"/api/projects/{project_id}/branches/{branch_id}/stats")
    return BranchStatsResponse(**result)

# Connection string endpoint
@app.get("/api/projects/{project_id}/branches/{branch_id}/connection", response_model=ConnectionStringResponse)
async def get_connection_string(
    project_id: str,
    branch_id: str,
    user: dict = Depends(get_current_user)
):
    """Get MongoDB connection string for a specific branch"""
    result = await call_go_engine("GET", f"/api/projects/{project_id}/branches/{branch_id}/connection")
    return ConnectionStringResponse(**result)

# CLI-specific endpoints that match Neon CLI patterns
@app.get("/api/me")
async def get_user_info(user: dict = Depends(get_current_user)):
    """Get current user information (Neon CLI compatibility)"""
    return {
        "id": user["user_id"],
        "email": "user@example.com",  # Mock for now
        "name": "Argon User",
        "created_at": datetime.utcnow()
    }

@app.get("/api/projects/{project_id}/connection_uri")
async def get_project_connection_uri(
    project_id: str,
    branch_id: Optional[str] = None,
    user: dict = Depends(get_current_user)
):
    """Get connection URI (Neon CLI compatibility)"""
    if not branch_id:
        # Get main branch
        branches_result = await call_go_engine("GET", f"/api/projects/{project_id}/branches")
        main_branch = next((b for b in branches_result["branches"] if b["is_main"]), None)
        if not main_branch:
            raise HTTPException(status_code=404, detail="No main branch found")
        branch_id = main_branch["id"]
    
    result = await call_go_engine("GET", f"/api/projects/{project_id}/branches/{branch_id}/connection")
    return {
        "uri": result["connection_string"],
        "database": result["database_name"],
        "branch_id": branch_id
    }

# Background task management
@app.post("/api/projects/{project_id}/sync")
async def trigger_sync(
    project_id: str,
    user: dict = Depends(get_current_user)
):
    """Trigger manual sync for a project"""
    result = await call_go_engine("POST", f"/api/projects/{project_id}/sync", {})
    return {"message": "Sync triggered", "job_id": result.get("job_id")}

# Development utilities
@app.get("/api/debug/engine-status")
async def get_engine_status():
    """Get Go engine status for debugging"""
    try:
        result = await call_go_engine("GET", "/debug/status")
        return result
    except Exception as e:
        return {"error": str(e), "status": "disconnected"}

if __name__ == "__main__":
    import uvicorn
    
    port = int(os.getenv("PORT", "3000"))
    uvicorn.run(
        "main:app",
        host="0.0.0.0",
        port=port,
        reload=True,
        log_level="info"
    )