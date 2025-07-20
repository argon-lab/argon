import React, { useState, useEffect } from 'react';
import { Activity, Database, AlertTriangle, CheckCircle, Clock, Zap, TrendingUp, Server } from 'lucide-react';
import { wal } from '../services/api';

export function WALMonitor() {
  const [metrics, setMetrics] = useState(null);
  const [health, setHealth] = useState(null);
  const [alerts, setAlerts] = useState([]);
  const [performance, setPerformance] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [autoRefresh, setAutoRefresh] = useState(true);

  useEffect(() => {
    fetchAllData();
    
    if (autoRefresh) {
      const interval = setInterval(fetchAllData, 5000); // Refresh every 5 seconds
      return () => clearInterval(interval);
    }
  }, [autoRefresh]);

  const fetchAllData = async () => {
    try {
      const [metricsRes, healthRes, alertsRes, performanceRes] = await Promise.all([
        wal.metrics(),
        wal.health(),
        wal.alerts(),
        wal.performance(),
      ]);

      setMetrics(metricsRes.data);
      setHealth(healthRes.data);
      setAlerts(alertsRes.data.alerts || []);
      setPerformance(performanceRes.data);
      setError(null);
    } catch (err) {
      setError('Failed to fetch WAL monitoring data');
      console.error('WAL monitoring error:', err);
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

  if (error) {
    return (
      <div className="bg-red-50 border border-red-200 rounded-lg p-6">
        <div className="flex items-center space-x-2">
          <AlertTriangle className="h-5 w-5 text-red-600" />
          <p className="text-red-700">{error}</p>
        </div>
        <button
          onClick={fetchAllData}
          className="mt-3 text-red-600 hover:text-red-800 underline"
        >
          Retry
        </button>
      </div>
    );
  }

  const getHealthStatus = () => {
    if (!health) return { status: 'unknown', color: 'gray' };
    
    if (health.isHealthy) {
      return { status: 'healthy', color: 'green' };
    } else if (health.errors?.length > 0) {
      return { status: 'error', color: 'red' };
    } else {
      return { status: 'warning', color: 'yellow' };
    }
  };

  const healthStatus = getHealthStatus();

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex justify-between items-center">
        <div>
          <h1 className="text-3xl font-bold text-gray-900">WAL Monitor</h1>
          <p className="text-gray-600">Real-time Write-Ahead Log monitoring and metrics</p>
        </div>
        <div className="flex items-center space-x-4">
          <label className="flex items-center space-x-2">
            <input
              type="checkbox"
              checked={autoRefresh}
              onChange={(e) => setAutoRefresh(e.target.checked)}
              className="rounded border-gray-300"
            />
            <span className="text-sm text-gray-700">Auto-refresh</span>
          </label>
          <button
            onClick={fetchAllData}
            className="bg-blue-600 hover:bg-blue-700 text-white px-4 py-2 rounded-lg flex items-center space-x-2"
          >
            <Activity size={16} />
            <span>Refresh</span>
          </button>
        </div>
      </div>

      {/* Health Status */}
      <div className="bg-white rounded-lg shadow p-6">
        <div className="flex items-center justify-between mb-4">
          <h2 className="text-lg font-semibold text-gray-900">System Health</h2>
          <div className={`flex items-center space-x-2 px-3 py-1 rounded-full text-sm font-medium bg-${healthStatus.color}-100 text-${healthStatus.color}-800`}>
            {healthStatus.status === 'healthy' ? (
              <CheckCircle size={16} />
            ) : (
              <AlertTriangle size={16} />
            )}
            <span className="capitalize">{healthStatus.status}</span>
          </div>
        </div>
        
        {health && (
          <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">{health.uptime || '0s'}</p>
              <p className="text-sm text-gray-600">Uptime</p>
            </div>
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">{health.activeConnections || 0}</p>
              <p className="text-sm text-gray-600">Active Connections</p>
            </div>
            <div className="text-center">
              <p className="text-2xl font-bold text-gray-900">{health.memoryUsage || '0 MB'}</p>
              <p className="text-sm text-gray-600">Memory Usage</p>
            </div>
          </div>
        )}
      </div>

      {/* Performance Metrics */}
      <div className="grid grid-cols-1 md:grid-cols-4 gap-6">
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Operations/sec</p>
              <p className="text-2xl font-bold text-gray-900">
                {metrics?.operationsPerSecond || '0'}
              </p>
            </div>
            <Zap className="h-12 w-12 text-blue-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Total WAL Entries</p>
              <p className="text-2xl font-bold text-gray-900">
                {metrics?.totalEntries?.toLocaleString() || '0'}
              </p>
            </div>
            <Database className="h-12 w-12 text-green-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Avg Response Time</p>
              <p className="text-2xl font-bold text-gray-900">
                {performance?.averageResponseTime || '0ms'}
              </p>
            </div>
            <Clock className="h-12 w-12 text-yellow-600" />
          </div>
        </div>
        
        <div className="bg-white rounded-lg shadow p-6">
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm text-gray-600">Success Rate</p>
              <p className="text-2xl font-bold text-gray-900">
                {((metrics?.successRate || 0) * 100).toFixed(1)}%
              </p>
            </div>
            <TrendingUp className="h-12 w-12 text-purple-600" />
          </div>
        </div>
      </div>

      {/* Alerts */}
      {alerts.length > 0 && (
        <div className="bg-white rounded-lg shadow">
          <div className="px-6 py-4 border-b border-gray-200">
            <h2 className="text-lg font-semibold text-gray-900 flex items-center space-x-2">
              <AlertTriangle className="h-5 w-5 text-red-600" />
              <span>Active Alerts ({alerts.length})</span>
            </h2>
          </div>
          <div className="divide-y divide-gray-200">
            {alerts.map((alert, index) => (
              <div key={index} className="px-6 py-4">
                <div className="flex items-center justify-between">
                  <div className="flex items-center space-x-3">
                    <div className={`w-3 h-3 rounded-full bg-${alert.severity === 'critical' ? 'red' : alert.severity === 'warning' ? 'yellow' : 'blue'}-500`}></div>
                    <div>
                      <h3 className="text-sm font-medium text-gray-900">{alert.title}</h3>
                      <p className="text-sm text-gray-600">{alert.message}</p>
                    </div>
                  </div>
                  <span className="text-xs text-gray-500">
                    {new Date(alert.timestamp).toLocaleTimeString()}
                  </span>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Recent Activity */}
      <div className="bg-white rounded-lg shadow">
        <div className="px-6 py-4 border-b border-gray-200">
          <h2 className="text-lg font-semibold text-gray-900">Recent WAL Activity</h2>
        </div>
        <div className="p-6">
          <div className="space-y-4">
            {metrics?.recentOperations?.map((op, index) => (
              <div key={index} className="flex items-center space-x-4 py-2">
                <div className="flex-shrink-0">
                  <div className="w-2 h-2 bg-blue-500 rounded-full"></div>
                </div>
                <div className="flex-1">
                  <div className="flex items-center justify-between">
                    <span className="text-sm font-medium text-gray-900">{op.operation}</span>
                    <span className="text-xs text-gray-500">{op.timestamp}</span>
                  </div>
                  <p className="text-xs text-gray-600">
                    {op.collection} • LSN {op.lsn} • {op.duration}ms
                  </p>
                </div>
              </div>
            )) || (
              <p className="text-gray-500 text-center py-8">No recent activity data available</p>
            )}
          </div>
        </div>
      </div>

      {/* System Resources */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-6">
        <div className="bg-white rounded-lg shadow p-6">
          <h3 className="text-lg font-semibold text-gray-900 mb-4 flex items-center space-x-2">
            <Server className="h-5 w-5" />
            <span>WAL Storage</span>
          </h3>
          <div className="space-y-3">
            <div className="flex justify-between">
              <span className="text-sm text-gray-600">Total Size</span>
              <span className="text-sm font-medium">{metrics?.walSize || '0 MB'}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-sm text-gray-600">Entries</span>
              <span className="text-sm font-medium">{metrics?.totalEntries?.toLocaleString() || '0'}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-sm text-gray-600">Compression Ratio</span>
              <span className="text-sm font-medium">{metrics?.compressionRatio || '1.0x'}</span>
            </div>
          </div>
        </div>

        <div className="bg-white rounded-lg shadow p-6">
          <h3 className="text-lg font-semibold text-gray-900 mb-4 flex items-center space-x-2">
            <TrendingUp className="h-5 w-5" />
            <span>Performance Trends</span>
          </h3>
          <div className="space-y-3">
            <div className="flex justify-between">
              <span className="text-sm text-gray-600">Peak Ops/sec (24h)</span>
              <span className="text-sm font-medium">{performance?.peakOpsPerSecond || '0'}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-sm text-gray-600">Avg Latency (24h)</span>
              <span className="text-sm font-medium">{performance?.averageLatency || '0ms'}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-sm text-gray-600">Error Rate (24h)</span>
              <span className="text-sm font-medium">{((performance?.errorRate || 0) * 100).toFixed(2)}%</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}