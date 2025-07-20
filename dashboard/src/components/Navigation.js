import React from 'react';
import { Link, useLocation } from 'react-router-dom';
import { Database, Home, Settings, User, GitBranch } from 'lucide-react';

export function Navigation() {
  const location = useLocation();

  const isActive = (path) => location.pathname === path;

  return (
    <nav className="bg-white shadow-sm border-b">
      <div className="container mx-auto px-4">
        <div className="flex justify-between items-center h-16">
          {/* Logo */}
          <Link to="/" className="flex items-center space-x-2">
            <div className="h-8 w-8 bg-blue-600 rounded-lg flex items-center justify-center">
              <Database className="h-5 w-5 text-white" />
            </div>
            <span className="text-xl font-bold text-gray-900">Argon</span>
          </Link>

          {/* Navigation Links */}
          <div className="hidden md:flex items-center space-x-8">
            <Link
              to="/"
              className={`flex items-center space-x-2 px-3 py-2 rounded-lg transition-colors ${
                isActive('/') 
                  ? 'bg-blue-100 text-blue-700' 
                  : 'text-gray-600 hover:text-gray-900'
              }`}
            >
              <Home size={20} />
              <span>Dashboard</span>
            </Link>
            
            <button className="flex items-center space-x-2 px-3 py-2 rounded-lg text-gray-600 hover:text-gray-900">
              <GitBranch size={20} />
              <span>Branches</span>
            </button>
            
            <button className="flex items-center space-x-2 px-3 py-2 rounded-lg text-gray-600 hover:text-gray-900">
              <Settings size={20} />
              <span>Settings</span>
            </button>
          </div>

          {/* User Menu */}
          <div className="flex items-center space-x-4">
            <div className="hidden md:block">
              <span className="text-sm text-gray-600">Welcome back!</span>
            </div>
            <button className="flex items-center space-x-2 px-3 py-2 rounded-lg text-gray-600 hover:text-gray-900">
              <User size={20} />
              <span className="hidden md:inline">Profile</span>
            </button>
          </div>
        </div>
      </div>
    </nav>
  );
}