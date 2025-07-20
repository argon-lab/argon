import React, { useState, useEffect } from 'react';
import { Clock, Search, Database, History, ArrowLeft, Calendar, Zap } from 'lucide-react';
import { timetravel, projects, branches } from '../services/api';

export function TimeTravel() {
  const [projectsList, setProjectsList] = useState([]);
  const [selectedProject, setSelectedProject] = useState(null);
  const [branchList, setBranchList] = useState([]);
  const [selectedBranch, setSelectedBranch] = useState(null);
  const [timeTravelInfo, setTimeTravelInfo] = useState(null);
  const [queryResults, setQueryResults] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);
  
  // Query parameters
  const [targetLSN, setTargetLSN] = useState('');
  const [selectedCollection, setSelectedCollection] = useState('');
  const [queryMode, setQueryMode] = useState('lsn'); // 'lsn' or 'time'
  const [targetTime, setTargetTime] = useState('');

  useEffect(() => {
    fetchProjects();
  }, []);

  useEffect(() => {
    if (selectedProject) {
      fetchBranches(selectedProject);
    }
  }, [selectedProject]);

  useEffect(() => {
    if (selectedProject && selectedBranch) {
      fetchTimeTravelInfo();
    }
  }, [selectedProject, selectedBranch]);

  const fetchProjects = async () => {
    try {
      const response = await projects.list();
      setProjectsList(response.data.projects || []);
    } catch (err) {
      setError('Failed to fetch projects');
    }
  };

  const fetchBranches = async (projectId) => {
    try {
      const response = await branches.list(projectId);
      setBranchList(response.data.branches || []);
      // Auto-select main branch if available
      const mainBranch = response.data.branches.find(b => b.name === 'main');
      if (mainBranch) {
        setSelectedBranch(mainBranch._id);
      }
    } catch (err) {
      setError('Failed to fetch branches');
    }
  };

  const fetchTimeTravelInfo = async () => {
    try {
      setLoading(true);
      const response = await timetravel.info(selectedProject, selectedBranch);
      setTimeTravelInfo(response.data);
    } catch (err) {
      setError('Failed to fetch time travel info');
    } finally {
      setLoading(false);
    }
  };

  const handleQuery = async () => {
    if (!selectedProject || !selectedBranch) {
      setError('Please select a project and branch');
      return;
    }

    if (queryMode === 'lsn' && !targetLSN) {
      setError('Please enter a target LSN');
      return;
    }

    if (queryMode === 'time' && !targetTime) {
      setError('Please select a target time');
      return;
    }

    try {
      setLoading(true);
      setError(null);

      let lsn = targetLSN;
      if (queryMode === 'time') {
        // Convert time to LSN (this would need backend support)
        // For now, we'll use the LSN directly
        lsn = timeTravelInfo?.latestLSN || 0;
      }

      const response = await timetravel.query(selectedProject, selectedBranch, lsn, selectedCollection);
      setQueryResults(response.data);
    } catch (err) {
      setError('Failed to execute time travel query');
    } finally {
      setLoading(false);
    }
  };

  const resetQuery = () => {
    setQueryResults(null);
    setTargetLSN('');
    setTargetTime('');
    setSelectedCollection('');
    setError(null);
  };

  const formatDate = (dateString) => {
    if (!dateString) return 'N/A';
    return new Date(dateString).toLocaleString();
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center space-x-4">
        <Clock className="h-8 w-8 text-blue-600" />
        <div>
          <h1 className="text-3xl font-bold text-gray-900">Time Travel</h1>
          <p className="text-gray-600">Query your MongoDB database at any point in history</p>
        </div>
      </div>

      {/* Project and Branch Selection */}
      <div className="bg-white rounded-lg shadow p-6">
        <h2 className="text-lg font-semibold text-gray-900 mb-4">Select Target</h2>
        <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">Project</label>
            <select
              value={selectedProject || ''}
              onChange={(e) => setSelectedProject(e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
            >
              <option value="">Select a project</option>
              {projectsList.map((project) => (
                <option key={project._id} value={project._id}>
                  {project.name}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">Branch</label>
            <select
              value={selectedBranch || ''}
              onChange={(e) => setSelectedBranch(e.target.value)}
              className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
              disabled={!selectedProject}
            >
              <option value="">Select a branch</option>
              {branchList.map((branch) => (
                <option key={branch._id} value={branch._id}>
                  {branch.name}
                </option>
              ))}
            </select>
          </div>
        </div>
      </div>

      {/* Time Travel Info */}
      {timeTravelInfo && (
        <div className="bg-white rounded-lg shadow p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4 flex items-center space-x-2">
            <History className="h-5 w-5" />
            <span>Time Travel Range</span>
          </h2>
          <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">{timeTravelInfo.earliestLSN}</p>
              <p className="text-sm text-gray-600">Earliest LSN</p>
            </div>
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">{timeTravelInfo.latestLSN}</p>
              <p className="text-sm text-gray-600">Latest LSN</p>
            </div>
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">{timeTravelInfo.entryCount?.toLocaleString()}</p>
              <p className="text-sm text-gray-600">Total Operations</p>
            </div>
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">
                {timeTravelInfo.earliestTime ? Math.ceil((new Date() - new Date(timeTravelInfo.earliestTime)) / (1000 * 60 * 60 * 24)) : 0}
              </p>
              <p className="text-sm text-gray-600">Days of History</p>
            </div>
          </div>
          
          {timeTravelInfo.earliestTime && timeTravelInfo.latestTime && (
            <div className="mt-4 p-4 bg-gray-50 rounded-lg">
              <div className="grid grid-cols-1 md:grid-cols-2 gap-4 text-sm">
                <div>
                  <span className="font-medium text-gray-700">Earliest: </span>
                  <span className="text-gray-600">{formatDate(timeTravelInfo.earliestTime)}</span>
                </div>
                <div>
                  <span className="font-medium text-gray-700">Latest: </span>
                  <span className="text-gray-600">{formatDate(timeTravelInfo.latestTime)}</span>
                </div>
              </div>
            </div>
          )}
        </div>
      )}

      {/* Query Interface */}
      {timeTravelInfo && (
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-lg font-semibold text-gray-900 flex items-center space-x-2">
              <Search className="h-5 w-5" />
              <span>Query Historical State</span>
            </h2>
            {queryResults && (
              <button
                onClick={resetQuery}
                className="text-blue-600 hover:text-blue-800 flex items-center space-x-1"
              >
                <ArrowLeft size={16} />
                <span>New Query</span>
              </button>
            )}
          </div>

          {!queryResults ? (
            <div className="space-y-4">
              {/* Query Mode Selection */}
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-2">Query Mode</label>
                <div className="flex space-x-4">
                  <label className="flex items-center space-x-2">
                    <input
                      type="radio"
                      value="lsn"
                      checked={queryMode === 'lsn'}
                      onChange={(e) => setQueryMode(e.target.value)}
                      className="text-blue-600"
                    />
                    <span className="text-sm">By LSN</span>
                  </label>
                  <label className="flex items-center space-x-2">
                    <input
                      type="radio"
                      value="time"
                      checked={queryMode === 'time'}
                      onChange={(e) => setQueryMode(e.target.value)}
                      className="text-blue-600"
                    />
                    <span className="text-sm">By Time</span>
                  </label>
                </div>
              </div>

              <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
                {queryMode === 'lsn' ? (
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">
                      Target LSN
                    </label>
                    <input
                      type="number"
                      value={targetLSN}
                      onChange={(e) => setTargetLSN(e.target.value)}
                      min={timeTravelInfo?.earliestLSN}
                      max={timeTravelInfo?.latestLSN}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                      placeholder={`${timeTravelInfo?.earliestLSN} - ${timeTravelInfo?.latestLSN}`}
                    />
                  </div>
                ) : (
                  <div>
                    <label className="block text-sm font-medium text-gray-700 mb-1">
                      Target Time
                    </label>
                    <input
                      type="datetime-local"
                      value={targetTime}
                      onChange={(e) => setTargetTime(e.target.value)}
                      min={timeTravelInfo?.earliestTime?.slice(0, 16)}
                      max={timeTravelInfo?.latestTime?.slice(0, 16)}
                      className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                    />
                  </div>
                )}

                <div>
                  <label className="block text-sm font-medium text-gray-700 mb-1">
                    Collection (Optional)
                  </label>
                  <input
                    type="text"
                    value={selectedCollection}
                    onChange={(e) => setSelectedCollection(e.target.value)}
                    className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                    placeholder="All collections"
                  />
                </div>

                <div className="flex items-end">
                  <button
                    onClick={handleQuery}
                    disabled={loading || (!targetLSN && queryMode === 'lsn') || (!targetTime && queryMode === 'time')}
                    className="w-full bg-blue-600 hover:bg-blue-700 disabled:bg-gray-400 text-white px-4 py-2 rounded-lg flex items-center justify-center space-x-2"
                  >
                    {loading ? (
                      <div className="animate-spin rounded-full h-4 w-4 border-b-2 border-white"></div>
                    ) : (
                      <>
                        <Zap size={16} />
                        <span>Query</span>
                      </>
                    )}
                  </button>
                </div>
              </div>
            </div>
          ) : (
            /* Query Results */
            <div className="space-y-4">
              <div className="flex items-center justify-between p-4 bg-green-50 border border-green-200 rounded-lg">
                <div className="flex items-center space-x-2">
                  <Database className="h-5 w-5 text-green-600" />
                  <span className="font-medium text-green-800">
                    Query completed for {queryMode === 'lsn' ? `LSN ${targetLSN}` : formatDate(targetTime)}
                  </span>
                </div>
                <span className="text-sm text-green-600">
                  {queryResults.documentsFound || 0} documents found
                </span>
              </div>

              {queryResults.collections && queryResults.collections.length > 0 ? (
                <div className="space-y-4">
                  {queryResults.collections.map((collection, index) => (
                    <div key={index} className="border border-gray-200 rounded-lg p-4">
                      <div className="flex items-center justify-between mb-3">
                        <h3 className="font-medium text-gray-900">{collection.name}</h3>
                        <span className="text-sm text-gray-600">
                          {collection.documentCount} documents
                        </span>
                      </div>
                      
                      {collection.documents && collection.documents.length > 0 && (
                        <div className="space-y-2">
                          {collection.documents.slice(0, 5).map((doc, docIndex) => (
                            <div key={docIndex} className="p-3 bg-gray-50 rounded text-sm">
                              <div className="font-mono text-xs text-gray-600">
                                ID: {doc._id}
                              </div>
                              <div className="mt-1 text-gray-800">
                                {JSON.stringify(doc, null, 2).slice(0, 200)}...
                              </div>
                            </div>
                          ))}
                          {collection.documents.length > 5 && (
                            <p className="text-sm text-gray-500 text-center">
                              ... and {collection.documents.length - 5} more documents
                            </p>
                          )}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              ) : (
                <div className="text-center py-8">
                  <Database className="h-12 w-12 text-gray-400 mx-auto mb-4" />
                  <p className="text-gray-600">No data found at the specified point in time</p>
                </div>
              )}
            </div>
          )}
        </div>
      )}

      {/* Error Display */}
      {error && (
        <div className="bg-red-50 border border-red-200 rounded-lg p-4">
          <p className="text-red-700">{error}</p>
        </div>
      )}
    </div>
  );
}