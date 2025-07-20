import React from 'react';
import { GitHub, Twitter, Globe } from 'lucide-react';

export function Footer() {
  return (
    <footer className="bg-white border-t border-gray-200 mt-12">
      <div className="container mx-auto px-4 py-8">
        <div className="grid grid-cols-1 md:grid-cols-4 gap-8">
          <div>
            <h3 className="text-lg font-semibold text-gray-900 mb-4">Argon</h3>
            <p className="text-gray-600 text-sm">
              Git-like MongoDB branching for ML/AI workflows. Built with ❤️ for the data science community.
            </p>
          </div>
          
          <div>
            <h4 className="text-sm font-semibold text-gray-900 mb-4">Product</h4>
            <ul className="space-y-2 text-sm">
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Features</a></li>
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Documentation</a></li>
              <li><a href="#" className="text-gray-600 hover:text-gray-900">API Reference</a></li>
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Pricing</a></li>
            </ul>
          </div>
          
          <div>
            <h4 className="text-sm font-semibold text-gray-900 mb-4">Resources</h4>
            <ul className="space-y-2 text-sm">
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Getting Started</a></li>
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Tutorials</a></li>
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Examples</a></li>
              <li><a href="#" className="text-gray-600 hover:text-gray-900">Community</a></li>
            </ul>
          </div>
          
          <div>
            <h4 className="text-sm font-semibold text-gray-900 mb-4">Connect</h4>
            <div className="flex space-x-4">
              <a href="#" className="text-gray-600 hover:text-gray-900">
                <GitHub size={20} />
              </a>
              <a href="#" className="text-gray-600 hover:text-gray-900">
                <Twitter size={20} />
              </a>
              <a href="#" className="text-gray-600 hover:text-gray-900">
                <Globe size={20} />
              </a>
            </div>
          </div>
        </div>
        
        <div className="border-t border-gray-200 pt-8 mt-8 flex flex-col md:flex-row justify-between items-center">
          <p className="text-gray-600 text-sm">
            © 2024 Argon. All rights reserved.
          </p>
          <div className="flex space-x-6 mt-4 md:mt-0">
            <a href="#" className="text-gray-600 hover:text-gray-900 text-sm">Privacy Policy</a>
            <a href="#" className="text-gray-600 hover:text-gray-900 text-sm">Terms of Service</a>
            <a href="#" className="text-gray-600 hover:text-gray-900 text-sm">Support</a>
          </div>
        </div>
      </div>
    </footer>
  );
}