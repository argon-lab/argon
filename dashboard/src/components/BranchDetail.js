import React, { useState, useEffect } from 'react';
import { useParams, Link } from 'react-router-dom';
import { ArrowLeft, GitBranch, Database, Activity, Calendar, BarChart3 } from 'lucide-react';
import { api } from '../services/api';

export function BranchDetail() {
  const { projectId, branchId } = useParams();
  const [branch, setBranch] = useState(null);
  const [stats, setStats] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  useEffect(() => {
    fetchBranch();
    fetchBranchStats();
  }, [projectId, branchId]);

  const fetchBranch = async () => {
    try {
      const response = await api.get(`/projects/${projectId}/branches/${branchId}`);
      setBranch(response.data.branch);
    } catch (err) {
      setError('Failed to fetch branch');
    }
  };

  const fetchBranchStats = async () => {
    try {
      const response = await api.get(`/projects/${projectId}/branches/${branchId}/stats`);
      setStats(response.data.stats);
    } catch (err) {
      // Stats endpoint might not exist yet, that's okay
      setStats({
        documents: 0,
        collections: 0,
        storageSize: 0,
        lastActivity: new Date().toISOString()
      });
    } finally {
      setLoading(false);
    }
  };

  if (loading) {
    return (
      <div className="flex justify-center items-center h-64">
        <div className="animate-spin rounded-full h-12 w-12 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  if (!branch) {
    return (
      <div className="text-center py-12">
        <h2 className="text-2xl font-bold text-gray-900">Branch not found</h2>
        <Link to={`/projects/${projectId}`} className="text-blue-600 hover:text-blue-800 mt-4 inline-block">
          ← Back to Project
        </Link>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div className="flex items-center space-x-4">
          <Link to={`/projects/${projectId}`} className="text-gray-600 hover:text-gray-800">
            <ArrowLeft size={24} />
          </Link>
          <div>
            <div className="flex items-center space-x-2">
              <h1 className="text-3xl font-bold text-gray-900">{branch.name}</h1>
              {branch.name === 'main' && (
                <span className="bg-blue-100 text-blue-800 text-sm px-3 py-1 rounded-full">
                  Main Branch
                </span>
              )}
              {branch.isActive && (
                <span className="bg-green-100 text-green-800 text-sm px-3 py-1 rounded-full">
                  Active
                </span>
              )}
            </div>
            <p className="text-gray-600">
              Created from {branch.parentBranch || 'main'} • {formatDate(branch.createdAt)}
            </p>
          </div>
        </div>
        <div className="flex space-x-2">
          <button className="bg-blue-600 hover:bg-blue-700 text-white px-4 py-2 rounded-lg">
            Connect to Branch
          </button>
          {branch.name !== 'main' && (
            <button className="bg-green-600 hover:bg-green-700 text-white px-4 py-2 rounded-lg">
              Merge Branch
            </button>
          )}
        </div>
      </div>

      {/* Error Message */}
      {error && (
        <div className="bg-red-50 border border-red-200 rounded-lg p-4">
          <p className="text-red-700">{error}</p>
        </div>
      )}

      {/* Branch Stats */}
      <div className="grid grid-cols-1 md:grid-cols-4 gap-6">
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Documents</p>
              <p className="text-2xl font-bold text-gray-900">
                {stats?.documents?.toLocaleString() || '0'}
              </p>
            </div>
            <Database className="h-12 w-12 text-blue-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Collections</p>
              <p className="text-2xl font-bold text-gray-900">
                {stats?.collections || '0'}
              </p>
            </div>
            <GitBranch className="h-12 w-12 text-green-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Storage Size</p>
              <p className="text-2xl font-bold text-gray-900">
                {formatBytes(stats?.storageSize || 0)}
              </p>
            </div>
            <BarChart3 className="h-12 w-12 text-purple-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Last Activity</p>
              <p className="text-lg font-bold text-gray-900">
                {formatDate(stats?.lastActivity || branch.updatedAt)}
              </p>
            </div>
            <Activity className="h-12 w-12 text-orange-600" />
          </div>
        </div>
      </div>

      {/* Connection Information */}
      <div className="bg-white rounded-lg shadow">
        <div className="px-6 py-4 border-b border-gray-200">
          <h2 className="text-lg font-semibold text-gray-900">Connection Information</h2>
        </div>
        <div className="px-6 py-4">
          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">
                MongoDB Connection String
              </label>
              <div className="flex items-center space-x-2">
                <code className="flex-1 bg-gray-50 px-3 py-2 rounded-lg text-sm font-mono">
                  mongodb://localhost:27017/argon_{branch.name}
                </code>
                <button className="px-3 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 text-sm">
                  Copy
                </button>
              </div>
            </div>
            
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">
                CLI Command
              </label>
              <div className="flex items-center space-x-2">
                <code className="flex-1 bg-gray-50 px-3 py-2 rounded-lg text-sm font-mono">
                  argon branch connect {branch.name}
                </code>
                <button className="px-3 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700 text-sm">
                  Copy
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      {/* Branch Operations */}
      <div className="bg-white rounded-lg shadow">
        <div className="px-6 py-4 border-b border-gray-200">
          <h2 className="text-lg font-semibold text-gray-900">Branch Operations</h2>
        </div>
        <div className="px-6 py-4">
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <button className="p-4 border border-gray-200 rounded-lg hover:bg-gray-50 text-left">
              <div className="flex items-center space-x-3">
                <Database className="h-8 w-8 text-blue-600" />
                <div>
                  <h3 className="font-medium text-gray-900">Export Branch Data</h3>
                  <p className="text-sm text-gray-600">Download branch data as JSON</p>
                </div>
              </div>
            </button>
            
            <button className="p-4 border border-gray-200 rounded-lg hover:bg-gray-50 text-left">
              <div className="flex items-center space-x-3">
                <Activity className="h-8 w-8 text-green-600" />
                <div>
                  <h3 className="font-medium text-gray-900">View Activity Log</h3>
                  <p className="text-sm text-gray-600">See all changes to this branch</p>
                </div>
              </div>
            </button>
            
            <button className="p-4 border border-gray-200 rounded-lg hover:bg-gray-50 text-left">
              <div className="flex items-center space-x-3">
                <BarChart3 className="h-8 w-8 text-purple-600" />
                <div>
                  <h3 className="font-medium text-gray-900">Performance Metrics</h3>
                  <p className="text-sm text-gray-600">View query performance stats</p>
                </div>
              </div>
            </button>
            
            <button className="p-4 border border-gray-200 rounded-lg hover:bg-gray-50 text-left">
              <div className="flex items-center space-x-3">
                <GitBranch className="h-8 w-8 text-orange-600" />
                <div>
                  <h3 className="font-medium text-gray-900">Compare Branches</h3>
                  <p className="text-sm text-gray-600">Diff with other branches</p>
                </div>
              </div>
            </button>
          </div>
        </div>
      </div>

      {/* Recent Activity */}
      <div className="bg-white rounded-lg shadow">
        <div className="px-6 py-4 border-b border-gray-200">
          <h2 className="text-lg font-semibold text-gray-900">Recent Activity</h2>
        </div>
        <div className="px-6 py-4">
          <div className="space-y-3">
            <div className="flex items-center space-x-3 text-sm">
              <div className="w-2 h-2 bg-green-500 rounded-full"></div>
              <span className="text-gray-900">Branch created from {branch.parentBranch || 'main'}</span>
              <span className="text-gray-500">{formatDate(branch.createdAt)}</span>
            </div>
            
            {stats?.lastActivity && (
              <div className="flex items-center space-x-3 text-sm">
                <div className="w-2 h-2 bg-blue-500 rounded-full"></div>
                <span className="text-gray-900">Data synchronized</span>
                <span className="text-gray-500">{formatDate(stats.lastActivity)}</span>
              </div>
            )}
            
            <div className="flex items-center space-x-3 text-sm text-gray-500">
              <div className="w-2 h-2 bg-gray-300 rounded-full"></div>
              <span>No recent activity</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function formatDate(dateString) {
  const date = new Date(dateString);
  const now = new Date();
  const diff = now - date;
  const minutes = Math.floor(diff / (1000 * 60));
  const hours = Math.floor(diff / (1000 * 60 * 60));
  const days = Math.floor(diff / (1000 * 60 * 60 * 24));
  
  if (minutes < 60) return `${minutes}m ago`;
  if (hours < 24) return `${hours}h ago`;
  if (days < 7) return `${days}d ago`;
  return date.toLocaleDateString();
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}