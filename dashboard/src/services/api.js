import axios from 'axios';

// Create axios instance with base configuration
const api = axios.create({
  baseURL: process.env.REACT_APP_API_URL || 'http://localhost:8080/api',
  timeout: 10000,
  headers: {
    'Content-Type': 'application/json',
  },
});

// Request interceptor to add auth token
api.interceptors.request.use(
  (config) => {
    const token = localStorage.getItem('argon_token');
    if (token) {
      config.headers.Authorization = `Bearer ${token}`;
    }
    return config;
  },
  (error) => {
    return Promise.reject(error);
  }
);

// Response interceptor to handle errors
api.interceptors.response.use(
  (response) => {
    return response;
  },
  (error) => {
    if (error.response?.status === 401) {
      // Handle unauthorized access
      localStorage.removeItem('argon_token');
      window.location.href = '/login';
    }
    
    if (error.response?.status === 429) {
      // Handle rate limiting
      const retryAfter = error.response.headers['retry-after'];
      if (retryAfter) {
        throw new Error(`Rate limit exceeded. Please try again in ${retryAfter} seconds.`);
      }
    }
    
    return Promise.reject(error);
  }
);

// API methods
export const auth = {
  login: (credentials) => api.post('/auth/login', credentials),
  register: (userData) => api.post('/auth/register', userData),
  logout: () => api.post('/auth/logout'),
  getCurrentUser: () => api.get('/auth/me'),
};

export const projects = {
  list: () => api.get('/projects'),
  get: (id) => api.get(`/projects/${id}`),
  create: (data) => api.post('/projects', data),
  update: (id, data) => api.put(`/projects/${id}`, data),
  delete: (id) => api.delete(`/projects/${id}`),
};

export const branches = {
  list: (projectId) => api.get(`/projects/${projectId}/branches`),
  get: (projectId, branchId) => api.get(`/projects/${projectId}/branches/${branchId}`),
  create: (projectId, data) => api.post(`/projects/${projectId}/branches`, data),
  update: (projectId, branchId, data) => api.put(`/projects/${projectId}/branches/${branchId}`, data),
  delete: (projectId, branchId) => api.delete(`/projects/${projectId}/branches/${branchId}`),
  merge: (projectId, branchId, targetBranch) => 
    api.post(`/projects/${projectId}/branches/${branchId}/merge`, { targetBranch }),
  stats: (projectId, branchId) => api.get(`/projects/${projectId}/branches/${branchId}/stats`),
};

export const data = {
  getCollections: (projectId, branchId) => 
    api.get(`/projects/${projectId}/branches/${branchId}/collections`),
  getDocuments: (projectId, branchId, collection, params = {}) => 
    api.get(`/projects/${projectId}/branches/${branchId}/collections/${collection}/documents`, { params }),
  createDocument: (projectId, branchId, collection, data) => 
    api.post(`/projects/${projectId}/branches/${branchId}/collections/${collection}/documents`, data),
  updateDocument: (projectId, branchId, collection, documentId, data) => 
    api.put(`/projects/${projectId}/branches/${branchId}/collections/${collection}/documents/${documentId}`, data),
  deleteDocument: (projectId, branchId, collection, documentId) => 
    api.delete(`/projects/${projectId}/branches/${branchId}/collections/${collection}/documents/${documentId}`),
};

export const stats = {
  getProject: (projectId) => api.get(`/projects/${projectId}/stats`),
  getBranch: (projectId, branchId) => api.get(`/projects/${projectId}/branches/${branchId}/stats`),
  getUsage: () => api.get('/stats/usage'),
};

export const system = {
  health: () => api.get('/health'),
  version: () => api.get('/version'),
  status: () => api.get('/status'),
};

// Export the axios instance for direct use
export { api };

// Helper functions
export const setAuthToken = (token) => {
  if (token) {
    localStorage.setItem('argon_token', token);
    api.defaults.headers.common['Authorization'] = `Bearer ${token}`;
  } else {
    localStorage.removeItem('argon_token');
    delete api.defaults.headers.common['Authorization'];
  }
};

export const getAuthToken = () => {
  return localStorage.getItem('argon_token');
};

export const isAuthenticated = () => {
  return !!getAuthToken();
};

// API error handler
export const handleApiError = (error) => {
  if (error.response) {
    // Server responded with error status
    const { status, data } = error.response;
    
    switch (status) {
      case 400:
        return data.message || 'Bad request';
      case 401:
        return 'Unauthorized. Please log in again.';
      case 403:
        return 'Access denied. You do not have permission to perform this action.';
      case 404:
        return 'Resource not found';
      case 429:
        return 'Rate limit exceeded. Please try again later.';
      case 500:
        return 'Internal server error. Please try again later.';
      default:
        return data.message || 'An unexpected error occurred';
    }
  } else if (error.request) {
    // Request was made but no response received
    return 'Network error. Please check your connection and try again.';
  } else {
    // Something else happened
    return error.message || 'An unexpected error occurred';
  }
};

// Mock API for development (when backend is not available)
export const mockApi = {
  projects: {
    list: () => Promise.resolve({
      data: {
        projects: [
          {
            _id: '1',
            name: 'ML Training Project',
            description: 'Training models with versioned datasets',
            createdAt: '2024-01-15T10:00:00Z',
            updatedAt: '2024-01-20T15:30:00Z',
            branchCount: 3,
            isActive: true,
          },
          {
            _id: '2',
            name: 'A/B Testing Database',
            description: 'User behavior analysis and testing',
            createdAt: '2024-01-10T09:00:00Z',
            updatedAt: '2024-01-18T14:20:00Z',
            branchCount: 5,
            isActive: true,
          },
        ],
      },
    }),
  },
  branches: {
    list: (projectId) => Promise.resolve({
      data: {
        branches: [
          {
            _id: '1',
            name: 'main',
            parentBranch: null,
            isActive: true,
            createdAt: '2024-01-15T10:00:00Z',
            updatedAt: '2024-01-20T15:30:00Z',
          },
          {
            _id: '2',
            name: 'feature-new-model',
            parentBranch: 'main',
            isActive: false,
            createdAt: '2024-01-18T11:00:00Z',
            updatedAt: '2024-01-19T16:45:00Z',
          },
        ],
      },
    }),
  },
};

// Use mock API if in development and no backend available
const isDevelopment = process.env.NODE_ENV === 'development';
const useMockApi = isDevelopment && !process.env.REACT_APP_API_URL;

if (useMockApi) {
  console.warn('Using mock API for development. Set REACT_APP_API_URL to use real backend.');
}