import React, { useState, useEffect } from 'react';
import { useParams, Link } from 'react-router-dom';
import { ArrowLeft, Plus, GitBranch, Calendar, Database, Settings, Trash2 } from 'lucide-react';
import { api } from '../services/api';

export function ProjectDetail() {
  const { projectId } = useParams();
  const [project, setProject] = useState(null);
  const [branches, setBranches] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [newBranch, setNewBranch] = useState({ name: '', parentBranch: 'main' });

  useEffect(() => {
    fetchProject();
    fetchBranches();
  }, [projectId]);

  const fetchProject = async () => {
    try {
      const response = await api.get(`/projects/${projectId}`);
      setProject(response.data.project);
    } catch (err) {
      setError('Failed to fetch project');
    }
  };

  const fetchBranches = async () => {
    try {
      const response = await api.get(`/projects/${projectId}/branches`);
      setBranches(response.data.branches || []);
    } catch (err) {
      setError('Failed to fetch branches');
    } finally {
      setLoading(false);
    }
  };

  const handleCreateBranch = async (e) => {
    e.preventDefault();
    try {
      await api.post(`/projects/${projectId}/branches`, newBranch);
      setShowCreateModal(false);
      setNewBranch({ name: '', parentBranch: 'main' });
      fetchBranches();
    } catch (err) {
      setError('Failed to create branch');
    }
  };

  const handleDeleteBranch = async (branchId) => {
    if (window.confirm('Are you sure you want to delete this branch?')) {
      try {
        await api.delete(`/projects/${projectId}/branches/${branchId}`);
        fetchBranches();
      } catch (err) {
        setError('Failed to delete branch');
      }
    }
  };

  if (loading) {
    return (
      <div className="flex justify-center items-center h-64">
        <div className="animate-spin rounded-full h-12 w-12 border-b-2 border-blue-600"></div>
      </div>
    );
  }

  if (!project) {
    return (
      <div className="text-center py-12">
        <h2 className="text-2xl font-bold text-gray-900">Project not found</h2>
        <Link to="/" className="text-blue-600 hover:text-blue-800 mt-4 inline-block">
          ← Back to Dashboard
        </Link>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div className="flex items-center space-x-4">
          <Link to="/" className="text-gray-600 hover:text-gray-800">
            <ArrowLeft size={24} />
          </Link>
          <div>
            <h1 className="text-3xl font-bold text-gray-900">{project.name}</h1>
            <p className="text-gray-600">{project.description}</p>
          </div>
        </div>
        <div className="flex space-x-2">
          <button
            onClick={() => setShowCreateModal(true)}
            className="bg-blue-600 hover:bg-blue-700 text-white px-4 py-2 rounded-lg flex items-center space-x-2"
          >
            <Plus size={20} />
            <span>New Branch</span>
          </button>
          <button className="bg-gray-200 hover:bg-gray-300 text-gray-700 px-4 py-2 rounded-lg flex items-center space-x-2">
            <Settings size={20} />
            <span>Settings</span>
          </button>
        </div>
      </div>

      {/* Error Message */}
      {error && (
        <div className="bg-red-50 border border-red-200 rounded-lg p-4">
          <p className="text-red-700">{error}</p>
        </div>
      )}

      {/* Project Stats */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Total Branches</p>
              <p className="text-2xl font-bold text-gray-900">{branches.length}</p>
            </div>
            <GitBranch className="h-12 w-12 text-green-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Active Branches</p>
              <p className="text-2xl font-bold text-gray-900">
                {branches.filter(b => b.isActive).length}
              </p>
            </div>
            <Database className="h-12 w-12 text-blue-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Created</p>
              <p className="text-lg font-bold text-gray-900">
                {formatDate(project.createdAt)}
              </p>
            </div>
            <Calendar className="h-12 w-12 text-purple-600" />
          </div>
        </div>
      </div>

      {/* Branches List */}
      <div className="bg-white rounded-lg shadow">
        <div className="px-6 py-4 border-b border-gray-200">
          <h2 className="text-lg font-semibold text-gray-900">Branches</h2>
        </div>
        
        {branches.length === 0 ? (
          <div className="px-6 py-12 text-center">
            <GitBranch className="h-12 w-12 text-gray-400 mx-auto mb-4" />
            <h3 className="text-lg font-medium text-gray-900 mb-2">No branches yet</h3>
            <p className="text-gray-600 mb-4">Create your first branch to start working with data</p>
            <button
              onClick={() => setShowCreateModal(true)}
              className="bg-blue-600 hover:bg-blue-700 text-white px-4 py-2 rounded-lg"
            >
              Create Branch
            </button>
          </div>
        ) : (
          <div className="divide-y divide-gray-200">
            {branches.map((branch) => (
              <div key={branch._id} className="px-6 py-4 hover:bg-gray-50">
                <div className="flex items-center justify-between">
                  <div className="flex items-center space-x-4">
                    <div className="flex-shrink-0">
                      <div className={`h-10 w-10 rounded-lg flex items-center justify-center ${
                        branch.isActive ? 'bg-green-100' : 'bg-gray-100'
                      }`}>
                        <GitBranch className={`h-6 w-6 ${
                          branch.isActive ? 'text-green-600' : 'text-gray-600'
                        }`} />
                      </div>
                    </div>
                    <div>
                      <div className="flex items-center space-x-2">
                        <h3 className="text-lg font-medium text-gray-900">{branch.name}</h3>
                        {branch.name === 'main' && (
                          <span className="bg-blue-100 text-blue-800 text-xs px-2 py-1 rounded-full">
                            Main
                          </span>
                        )}
                        {branch.isActive && (
                          <span className="bg-green-100 text-green-800 text-xs px-2 py-1 rounded-full">
                            Active
                          </span>
                        )}
                      </div>
                      <p className="text-sm text-gray-600">
                        Created from {branch.parentBranch || 'main'} • {formatDate(branch.createdAt)}
                      </p>
                    </div>
                  </div>
                  <div className="flex items-center space-x-2">
                    <Link
                      to={`/projects/${projectId}/branches/${branch._id}`}
                      className="text-blue-600 hover:text-blue-800 text-sm"
                    >
                      View Details
                    </Link>
                    {branch.name !== 'main' && (
                      <button
                        onClick={() => handleDeleteBranch(branch._id)}
                        className="text-red-600 hover:text-red-800 p-1"
                      >
                        <Trash2 size={16} />
                      </button>
                    )}
                  </div>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Create Branch Modal */}
      {showCreateModal && (
        <div className="fixed inset-0 bg-gray-600 bg-opacity-50 flex items-center justify-center z-50">
          <div className="bg-white rounded-lg shadow-xl p-6 w-full max-w-md">
            <div className="flex justify-between items-center mb-4">
              <h2 className="text-xl font-bold text-gray-900">Create New Branch</h2>
              <button
                onClick={() => setShowCreateModal(false)}
                className="text-gray-400 hover:text-gray-600"
              >
                ×
              </button>
            </div>
            
            <form onSubmit={handleCreateBranch} className="space-y-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  Branch Name
                </label>
                <input
                  type="text"
                  value={newBranch.name}
                  onChange={(e) => setNewBranch({ ...newBranch, name: e.target.value })}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                  placeholder="feature-branch"
                  required
                />
              </div>
              
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  Parent Branch
                </label>
                <select
                  value={newBranch.parentBranch}
                  onChange={(e) => setNewBranch({ ...newBranch, parentBranch: e.target.value })}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                >
                  {branches.map((branch) => (
                    <option key={branch._id} value={branch.name}>
                      {branch.name}
                    </option>
                  ))}
                </select>
              </div>
              
              <div className="flex space-x-3">
                <button
                  type="button"
                  onClick={() => setShowCreateModal(false)}
                  className="flex-1 px-4 py-2 border border-gray-300 rounded-lg text-gray-700 hover:bg-gray-50"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  className="flex-1 px-4 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700"
                >
                  Create Branch
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  );
}

function formatDate(dateString) {
  const date = new Date(dateString);
  return date.toLocaleDateString();
}