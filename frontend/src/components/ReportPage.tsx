import React, { useEffect, useState } from 'react';

const API_URL = process.env.REACT_APP_API_URL;

interface ReportRow {
  date: string;
  events_count: number;
  avg_value: number;
  min_value: number;
  max_value: number;
}

const ReportPage: React.FC = () => {
  const [initialized, setInitialized] = useState(false);
  const [authenticated, setAuthenticated] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [report, setReport] = useState<ReportRow[] | null>(null);

  useEffect(() => {
    fetch(`${API_URL}/auth/me`, { credentials: 'include' })
      .then((response) => setAuthenticated(response.ok))
      .catch(() => setAuthenticated(false))
      .finally(() => setInitialized(true));
  }, []);

  const login = () => {
    window.location.assign(`${API_URL}/auth/login?returnTo=${encodeURIComponent(window.location.pathname)}`);
  };

  const downloadReport = async () => {
    try {
      setLoading(true);
      setError(null);

      const response = await fetch(`${API_URL}/api/reports`, {
        credentials: 'include'
      });

      if (response.status === 401) {
        setAuthenticated(false);
        return;
      }
      if (!response.ok) {
        throw new Error(`Report request failed: ${response.status}`);
      }

      setReport(await response.json());
    } catch (err) {
      setError(err instanceof Error ? err.message : 'An error occurred');
    } finally {
      setLoading(false);
    }
  };

  if (!initialized) {
    return <div>Loading...</div>;
  }

  if (!authenticated) {
    return (
      <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
        <button
          onClick={login}
          className="px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600"
        >
          Login
        </button>
      </div>
    );
  }

  return (
    <div className="flex flex-col items-center justify-center min-h-screen bg-gray-100">
      <div className="p-8 bg-white rounded-lg shadow-md">
        <h1 className="text-2xl font-bold mb-6">Usage Reports</h1>

        <button
          onClick={downloadReport}
          disabled={loading}
          className={`px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600 ${
            loading ? 'opacity-50 cursor-not-allowed' : ''
          }`}
        >
          {loading ? 'Generating Report...' : 'Download Report'}
        </button>

        {error && (
          <div className="mt-4 p-4 bg-red-100 text-red-700 rounded">
            {error}
          </div>
        )}

        {report && report.length === 0 && (
          <p className="mt-6 text-gray-600">No data.</p>
        )}

        {report && report.length > 0 && (
          <table className="mt-6 text-left">
            <thead>
              <tr className="border-b">
                <th className="px-3 py-2">Date</th>
                <th className="px-3 py-2">Events</th>
                <th className="px-3 py-2">Avg</th>
                <th className="px-3 py-2">Min</th>
                <th className="px-3 py-2">Max</th>
              </tr>
            </thead>
            <tbody>
              {report.map((row) => (
                <tr key={row.date} className="border-b">
                  <td className="px-3 py-2">{row.date.slice(0, 10)}</td>
                  <td className="px-3 py-2">{row.events_count}</td>
                  <td className="px-3 py-2">{row.avg_value.toFixed(2)}</td>
                  <td className="px-3 py-2">{row.min_value}</td>
                  <td className="px-3 py-2">{row.max_value}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
};

export default ReportPage;
