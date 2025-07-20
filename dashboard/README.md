# Argon Dashboard

A modern web dashboard for managing MongoDB branches and projects with Argon.

## Features

- **Project Management**: Create, view, and manage your MongoDB projects with time travel
- **Branch Visualization**: Visual interface for branch operations and instant branching
- **WAL Monitoring**: Real-time Write-Ahead Log metrics, health monitoring, and performance analytics
- **Time Travel Interface**: Query your database at any point in history with visual timeline
- **MongoDB Import**: Import existing MongoDB databases with step-by-step wizard
- **Real-time Stats**: Live statistics and performance metrics with auto-refresh
- **Responsive Design**: Works on desktop, tablet, and mobile
- **Modern UI**: Clean, intuitive interface built with React and Tailwind CSS

## Quick Start

### Prerequisites

- Node.js 16+ 
- npm or yarn
- Argon CLI installed and configured
- MongoDB instance running (for WAL functionality)

### Installation

```bash
# Clone the repository
git clone https://github.com/argon-lab/argon.git
cd argon

# Start the API server (in one terminal)
cd api
go run main.go

# Start the dashboard (in another terminal)
cd dashboard
npm install
npm start
```

The dashboard will be available at `http://localhost:3000` and the API at `http://localhost:8080`.

### Configuration

Create a `.env` file in the dashboard directory:

```bash
# API Configuration
REACT_APP_API_URL=http://localhost:8080/api

# Optional: Enable debug mode
REACT_APP_DEBUG=true

# Optional: Set custom app title
REACT_APP_TITLE=Argon Dashboard
```

## Available Scripts

### `npm start`
Runs the app in development mode. Open [http://localhost:3000](http://localhost:3000) to view it in the browser.

### `npm test`
Launches the test runner in interactive watch mode.

### `npm run build`
Builds the app for production to the `build` folder. The build is optimized and ready for deployment.

### `npm run eject`
**Note: This is a one-way operation. Once you eject, you can't go back!**

## Project Structure

```
dashboard/
├── public/
│   ├── index.html
│   └── favicon.ico
├── src/
│   ├── components/
│   │   ├── Dashboard.js          # Main dashboard view
│   │   ├── ProjectDetail.js      # Project detail view
│   │   ├── BranchDetail.js       # Branch detail view
│   │   ├── Navigation.js         # Navigation component
│   │   └── Footer.js             # Footer component
│   ├── services/
│   │   └── api.js                # API service layer
│   ├── App.js                    # Main app component
│   ├── App.css                   # App styles
│   └── index.js                  # Entry point
├── package.json
└── README.md
```

## API Integration

The dashboard communicates with the Argon backend API. Make sure your backend is running and accessible.

### API Endpoints

- `GET /api/projects` - List all projects
- `POST /api/projects` - Create a new project
- `GET /api/projects/:id` - Get project details
- `GET /api/projects/:id/branches` - List project branches
- `POST /api/projects/:id/branches` - Create a new branch

### Authentication

The dashboard supports token-based authentication. Set the `REACT_APP_API_URL` environment variable to point to your Argon backend.

## Development

### Adding New Components

1. Create a new file in `src/components/`
2. Export your component as a named export
3. Import and use in your desired location

### Styling

The dashboard uses Tailwind CSS for styling. You can customize the design by:

1. Modifying the Tailwind configuration
2. Adding custom CSS in `App.css`
3. Using Tailwind utility classes in components

### State Management

The dashboard uses React's built-in state management. For more complex state requirements, consider adding:

- React Context for global state
- React Query for server state
- Zustand or Redux for client state

## Deployment

### Build for Production

```bash
npm run build
```

### Deploy to Static Hosting

The built files in the `build` folder can be deployed to any static hosting service:

- **Netlify**: Connect your Git repository and deploy automatically
- **Vercel**: Deploy with zero configuration
- **GitHub Pages**: Use the `gh-pages` package
- **AWS S3**: Upload the build folder to an S3 bucket

### Deploy with Docker

```dockerfile
# Build stage
FROM node:18-alpine as build
WORKDIR /app
COPY package*.json ./
RUN npm ci --only=production
COPY . .
RUN npm run build

# Production stage
FROM nginx:alpine
COPY --from=build /app/build /usr/share/nginx/html
COPY nginx.conf /etc/nginx/nginx.conf
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
```

## Browser Support

- Chrome (latest)
- Firefox (latest)
- Safari (latest)
- Edge (latest)

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](../LICENSE) file for details.

## Support

- **Documentation**: [../docs/](../docs/)
- **Issues**: [GitHub Issues](https://github.com/argon-lab/argon/issues)

## Features Overview

### 🔍 WAL Monitor
- Real-time performance metrics (ops/sec, response times, success rates)
- System health monitoring with alerts
- Memory usage and connection tracking
- Recent activity feed with operation details
- Auto-refresh capabilities

### ⏰ Time Travel
- Query database state at any LSN (Log Sequence Number)
- Historical data visualization
- Collection-level and document-level queries
- Time range selection with calendar picker
- Export historical data

### 📤 MongoDB Import
- Step-by-step import wizard with preview
- Batch processing with configurable sizes
- Dry-run capabilities for safe testing
- Progress tracking and status monitoring
- Error handling and validation

### 📊 Project & Branch Management
- Visual project overview with statistics
- Instant branch creation and management
- Branch comparison and merging
- Real-time collaboration features

## Roadmap

- [ ] Real-time notifications and websocket integration
- [ ] Advanced branch comparison with diff views
- [ ] Interactive data visualization charts
- [ ] Team collaboration features with user management
- [ ] Dark mode support and theme customization
- [ ] Mobile app (React Native)
- [ ] Advanced time travel with natural language queries
- [ ] Automated backup and restore scheduling
- [ ] Performance optimization suggestions

---

Built with ❤️ by the Argon team