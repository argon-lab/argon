import React from 'react';
import { BrowserRouter as Router, Routes, Route } from 'react-router-dom';
import { Dashboard } from './components/Dashboard';
import { ProjectDetail } from './components/ProjectDetail';
import { BranchDetail } from './components/BranchDetail';
import { WALMonitor } from './components/WALMonitor';
import { TimeTravel } from './components/TimeTravel';
import { ImportData } from './components/ImportData';
import { Navigation } from './components/Navigation';
import { Footer } from './components/Footer';
import './App.css';

function App() {
  return (
    <Router>
      <div className="min-h-screen bg-gray-50">
        <Navigation />
        <main className="container mx-auto px-4 py-8">
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/projects/:projectId" element={<ProjectDetail />} />
            <Route path="/projects/:projectId/branches/:branchId" element={<BranchDetail />} />
            <Route path="/monitor" element={<WALMonitor />} />
            <Route path="/timetravel" element={<TimeTravel />} />
            <Route path="/import" element={<ImportData />} />
          </Routes>
        </main>
        <Footer />
      </div>
    </Router>
  );
}

export default App;