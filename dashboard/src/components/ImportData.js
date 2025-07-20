import React, { useState } from 'react';
import { Upload, Database, Eye, CheckCircle, AlertCircle, ArrowRight, Loader } from 'lucide-react';
import { imports } from '../services/api';

export function ImportData() {
  const [step, setStep] = useState(1); // 1: Configure, 2: Preview, 3: Import, 4: Complete
  const [importConfig, setImportConfig] = useState({
    mongoUri: 'mongodb://localhost:27017',
    databaseName: '',
    projectName: '',
    batchSize: 1000,
    dryRun: false,
  });
  const [previewData, setPreviewData] = useState(null);
  const [importResult, setImportResult] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(null);

  const handlePreview = async (e) => {
    e.preventDefault();
    if (!importConfig.mongoUri || !importConfig.databaseName) {
      setError('MongoDB URI and database name are required');
      return;
    }

    try {
      setLoading(true);
      setError(null);
      
      const response = await imports.preview(importConfig.mongoUri, importConfig.databaseName);
      setPreviewData(response.data);
      setStep(2);
    } catch (err) {
      setError('Failed to preview import: ' + (err.response?.data?.message || err.message));
    } finally {
      setLoading(false);
    }
  };

  const handleImport = async () => {
    if (!importConfig.projectName) {
      setError('Project name is required for import');
      return;
    }

    try {
      setLoading(true);
      setError(null);
      setStep(3);

      const response = await imports.database(
        importConfig.mongoUri,
        importConfig.databaseName,
        importConfig.projectName,
        {
          batchSize: importConfig.batchSize,
          dryRun: importConfig.dryRun,
        }
      );
      
      setImportResult(response.data);
      setStep(4);
    } catch (err) {
      setError('Failed to import database: ' + (err.response?.data?.message || err.message));
      setStep(2); // Go back to preview
    } finally {
      setLoading(false);
    }
  };

  const resetImport = () => {
    setStep(1);
    setImportConfig({
      mongoUri: 'mongodb://localhost:27017',
      databaseName: '',
      projectName: '',
      batchSize: 1000,
      dryRun: false,
    });
    setPreviewData(null);
    setImportResult(null);
    setError(null);
  };

  const formatBytes = (bytes) => {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
  };

  const formatNumber = (num) => {
    if (num < 1000) return num.toString();
    if (num < 1000000) return (num / 1000).toFixed(1) + 'K';
    return (num / 1000000).toFixed(1) + 'M';
  };

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center space-x-4">
        <Upload className="h-8 w-8 text-blue-600" />
        <div>
          <h1 className="text-3xl font-bold text-gray-900">Import MongoDB Database</h1>
          <p className="text-gray-600">Bring your existing MongoDB data into Argon with time travel capabilities</p>
        </div>
      </div>

      {/* Progress Steps */}
      <div className="bg-white rounded-lg shadow p-6">
        <div className="flex items-center justify-between">
          {[
            { num: 1, label: 'Configure', icon: Database },
            { num: 2, label: 'Preview', icon: Eye },
            { num: 3, label: 'Import', icon: Upload },
            { num: 4, label: 'Complete', icon: CheckCircle },
          ].map((stepItem, index) => {
            const Icon = stepItem.icon;
            const isActive = step === stepItem.num;
            const isCompleted = step > stepItem.num;
            
            return (
              <div key={stepItem.num} className="flex items-center">
                <div className={`flex items-center justify-center w-10 h-10 rounded-full border-2 ${
                  isCompleted 
                    ? 'bg-green-500 border-green-500 text-white' 
                    : isActive 
                    ? 'bg-blue-500 border-blue-500 text-white'
                    : 'border-gray-300 text-gray-500'
                }`}>
                  <Icon size={20} />
                </div>
                <span className={`ml-2 text-sm font-medium ${
                  isActive ? 'text-blue-600' : isCompleted ? 'text-green-600' : 'text-gray-500'
                }`}>
                  {stepItem.label}
                </span>
                {index < 3 && (
                  <ArrowRight className={`mx-4 h-5 w-5 ${
                    step > stepItem.num ? 'text-green-500' : 'text-gray-300'
                  }`} />
                )}
              </div>
            );
          })}
        </div>
      </div>

      {/* Error Display */}
      {error && (
        <div className="bg-red-50 border border-red-200 rounded-lg p-4 flex items-center space-x-2">
          <AlertCircle className="h-5 w-5 text-red-600" />
          <p className="text-red-700">{error}</p>
        </div>
      )}

      {/* Step 1: Configuration */}
      {step === 1 && (
        <div className="bg-white rounded-lg shadow p-6">
          <h2 className="text-lg font-semibold text-gray-900 mb-4">Database Configuration</h2>
          <form onSubmit={handlePreview} className="space-y-4">
            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  MongoDB URI
                </label>
                <input
                  type="text"
                  value={importConfig.mongoUri}
                  onChange={(e) => setImportConfig({ ...importConfig, mongoUri: e.target.value })}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                  placeholder="mongodb://localhost:27017"
                  required
                />
                <p className="text-xs text-gray-500 mt-1">Connection string to your MongoDB instance</p>
              </div>
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  Database Name
                </label>
                <input
                  type="text"
                  value={importConfig.databaseName}
                  onChange={(e) => setImportConfig({ ...importConfig, databaseName: e.target.value })}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                  placeholder="my-database"
                  required
                />
                <p className="text-xs text-gray-500 mt-1">Name of the database to import</p>
              </div>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  Argon Project Name
                </label>
                <input
                  type="text"
                  value={importConfig.projectName}
                  onChange={(e) => setImportConfig({ ...importConfig, projectName: e.target.value })}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                  placeholder="imported-database"
                />
                <p className="text-xs text-gray-500 mt-1">Name for the new Argon project (can be set later)</p>
              </div>
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">
                  Batch Size
                </label>
                <input
                  type="number"
                  value={importConfig.batchSize}
                  onChange={(e) => setImportConfig({ ...importConfig, batchSize: parseInt(e.target.value) })}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                  min="100"
                  max="10000"
                />
                <p className="text-xs text-gray-500 mt-1">Documents processed per batch (100-10000)</p>
              </div>
            </div>

            <div className="flex items-center space-x-2">
              <input
                type="checkbox"
                id="dryRun"
                checked={importConfig.dryRun}
                onChange={(e) => setImportConfig({ ...importConfig, dryRun: e.target.checked })}
                className="rounded border-gray-300"
              />
              <label htmlFor="dryRun" className="text-sm text-gray-700">
                Dry run (preview import without making changes)
              </label>
            </div>

            <div className="flex justify-end">
              <button
                type="submit"
                disabled={loading}
                className="bg-blue-600 hover:bg-blue-700 disabled:bg-gray-400 text-white px-6 py-2 rounded-lg flex items-center space-x-2"
              >
                {loading ? (
                  <Loader className="animate-spin h-4 w-4" />
                ) : (
                  <Eye size={16} />
                )}
                <span>Preview Import</span>
              </button>
            </div>
          </form>
        </div>
      )}

      {/* Step 2: Preview */}
      {step === 2 && previewData && (
        <div className="space-y-6">
          <div className="bg-white rounded-lg shadow p-6">
            <div className="flex items-center justify-between mb-4">
              <h2 className="text-lg font-semibold text-gray-900">Import Preview</h2>
              <button
                onClick={() => setStep(1)}
                className="text-blue-600 hover:text-blue-800"
              >
                ← Back to Configuration
              </button>
            </div>

            {/* Summary Stats */}
            <div className="grid grid-cols-1 md:grid-cols-4 gap-4 mb-6">
              <div className="text-center p-4 bg-blue-50 rounded-lg">
                <p className="text-2xl font-bold text-blue-600">{previewData.collections?.length || 0}</p>
                <p className="text-sm text-gray-600">Collections</p>
              </div>
              <div className="text-center p-4 bg-green-50 rounded-lg">
                <p className="text-2xl font-bold text-green-600">{formatNumber(previewData.totalDocuments || 0)}</p>
                <p className="text-sm text-gray-600">Documents</p>
              </div>
              <div className="text-center p-4 bg-yellow-50 rounded-lg">
                <p className="text-2xl font-bold text-yellow-600">{formatBytes(previewData.estimatedSize || 0)}</p>
                <p className="text-sm text-gray-600">Estimated Size</p>
              </div>
              <div className="text-center p-4 bg-purple-50 rounded-lg">
                <p className="text-2xl font-bold text-purple-600">{formatNumber(previewData.estimatedWALEntries || 0)}</p>
                <p className="text-sm text-gray-600">WAL Entries</p>
              </div>
            </div>

            {/* Collections Detail */}
            <div>
              <h3 className="text-lg font-medium text-gray-900 mb-3">Collections to Import</h3>
              <div className="space-y-3">
                {previewData.collections?.map((collection, index) => (
                  <div key={index} className="border border-gray-200 rounded-lg p-4">
                    <div className="flex items-center justify-between">
                      <div>
                        <h4 className="font-medium text-gray-900">{collection.name}</h4>
                        <p className="text-sm text-gray-600">
                          {formatNumber(collection.documentCount)} documents • {formatBytes(collection.sizeBytes)} • {collection.indexCount} indexes
                        </p>
                      </div>
                      <CheckCircle className="h-5 w-5 text-green-500" />
                    </div>
                  </div>
                ))}
              </div>
            </div>

            {/* Import Actions */}
            <div className="flex justify-between items-center mt-6 pt-6 border-t border-gray-200">
              <div>
                {!importConfig.projectName && (
                  <div className="mb-4">
                    <label className="block text-sm font-medium text-gray-700 mb-1">
                      Project Name (Required for Import)
                    </label>
                    <input
                      type="text"
                      value={importConfig.projectName}
                      onChange={(e) => setImportConfig({ ...importConfig, projectName: e.target.value })}
                      className="px-3 py-2 border border-gray-300 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500"
                      placeholder="Enter project name"
                    />
                  </div>
                )}
              </div>
              <div className="flex space-x-3">
                <button
                  onClick={() => setImportConfig({ ...importConfig, dryRun: true })}
                  className="px-4 py-2 border border-gray-300 rounded-lg text-gray-700 hover:bg-gray-50"
                >
                  Dry Run
                </button>
                <button
                  onClick={handleImport}
                  disabled={!importConfig.projectName || loading}
                  className="bg-blue-600 hover:bg-blue-700 disabled:bg-gray-400 text-white px-6 py-2 rounded-lg flex items-center space-x-2"
                >
                  <Upload size={16} />
                  <span>Start Import</span>
                </button>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Step 3: Import Progress */}
      {step === 3 && (
        <div className="bg-white rounded-lg shadow p-6">
          <div className="text-center">
            <Loader className="animate-spin h-12 w-12 text-blue-600 mx-auto mb-4" />
            <h2 className="text-lg font-semibold text-gray-900 mb-2">Importing Database...</h2>
            <p className="text-gray-600">
              {importConfig.dryRun 
                ? 'Running dry run analysis...' 
                : `Importing ${previewData?.totalDocuments?.toLocaleString() || 0} documents into Argon...`
              }
            </p>
            <div className="mt-4 text-sm text-gray-500">
              This may take several minutes depending on the size of your database.
            </div>
          </div>
        </div>
      )}

      {/* Step 4: Complete */}
      {step === 4 && importResult && (
        <div className="space-y-6">
          <div className="bg-white rounded-lg shadow p-6">
            <div className="text-center mb-6">
              <CheckCircle className="h-16 w-16 text-green-500 mx-auto mb-4" />
              <h2 className="text-2xl font-bold text-gray-900 mb-2">
                {importConfig.dryRun ? 'Dry Run Complete!' : 'Import Complete!'}
              </h2>
              <p className="text-gray-600">
                {importConfig.dryRun 
                  ? 'Your import has been validated and is ready to proceed.'
                  : 'Your MongoDB database has been successfully imported with time travel capabilities.'
                }
              </p>
            </div>

            {/* Results Summary */}
            <div className="grid grid-cols-1 md:grid-cols-4 gap-4 mb-6">
              <div className="text-center p-4 bg-green-50 rounded-lg">
                <p className="text-2xl font-bold text-green-600">{formatNumber(importResult.importedDocs || 0)}</p>
                <p className="text-sm text-gray-600">Documents {importConfig.dryRun ? 'Analyzed' : 'Imported'}</p>
              </div>
              <div className="text-center p-4 bg-blue-50 rounded-lg">
                <p className="text-2xl font-bold text-blue-600">{formatNumber(importResult.walEntries || 0)}</p>
                <p className="text-sm text-gray-600">WAL Entries {importConfig.dryRun ? 'Would Create' : 'Created'}</p>
              </div>
              <div className="text-center p-4 bg-purple-50 rounded-lg">
                <p className="text-2xl font-bold text-purple-600">{importResult.collections?.length || 0}</p>
                <p className="text-sm text-gray-600">Collections</p>
              </div>
              <div className="text-center p-4 bg-yellow-50 rounded-lg">
                <p className="text-2xl font-bold text-yellow-600">{importResult.duration || '0ms'}</p>
                <p className="text-sm text-gray-600">Duration</p>
              </div>
            </div>

            {/* Project Info */}
            {!importConfig.dryRun && importResult.projectId && (
              <div className="bg-gray-50 rounded-lg p-4 mb-6">
                <h3 className="font-medium text-gray-900 mb-2">Project Details</h3>
                <div className="text-sm space-y-1">
                  <div><span className="font-medium">Project ID:</span> {importResult.projectId}</div>
                  <div><span className="font-medium">Branch ID:</span> {importResult.branchId}</div>
                  <div><span className="font-medium">LSN Range:</span> {importResult.startLSN} - {importResult.endLSN}</div>
                </div>
              </div>
            )}

            {/* Next Steps */}
            <div className="border-t border-gray-200 pt-6">
              <h3 className="font-medium text-gray-900 mb-3">Next Steps</h3>
              <div className="space-y-2 text-sm text-gray-600">
                {importConfig.dryRun ? (
                  <>
                    <div>• Remove the dry-run option to perform the actual import</div>
                    <div>• Review the import configuration if needed</div>
                  </>
                ) : (
                  <>
                    <div>• Use time travel to query historical states of your data</div>
                    <div>• Create branches for safe testing and experimentation</div>
                    <div>• Set up restore points for disaster recovery</div>
                  </>
                )}
              </div>
            </div>

            {/* Actions */}
            <div className="flex justify-center space-x-4 mt-6">
              <button
                onClick={resetImport}
                className="px-6 py-2 border border-gray-300 rounded-lg text-gray-700 hover:bg-gray-50"
              >
                Import Another Database
              </button>
              {!importConfig.dryRun && (
                <button className="px-6 py-2 bg-blue-600 text-white rounded-lg hover:bg-blue-700">
                  View Project
                </button>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
}