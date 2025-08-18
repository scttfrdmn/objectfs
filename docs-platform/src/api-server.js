#!/usr/bin/env node

/**
 * ObjectFS Documentation API Server
 *
 * Provides API endpoints for the documentation platform including:
 * - API playground proxy
 * - Code execution sandbox
 * - Interactive tutorials
 * - Real-time examples
 */

const express = require('express');
const cors = require('cors');
const helmet = require('helmet');
const compression = require('compression');
const morgan = require('morgan');
const { createServer } = require('http');
const { Server } = require('socket.io');
const swaggerUi = require('swagger-ui-express');
const swaggerJsdoc = require('swagger-jsdoc');
const Docker = require('dockerode');
const path = require('path');
const fs = require('fs').promises;

// Load environment variables
require('dotenv').config();

const app = express();
const server = createServer(app);
const io = new Server(server, {
  cors: {
    origin: process.env.NODE_ENV === 'production' ? false : "*",
    methods: ["GET", "POST"]
  }
});

const docker = new Docker();
const PORT = process.env.API_PORT || 3001;
const OBJECTFS_API_BASE = process.env.OBJECTFS_API_BASE || 'http://localhost:8081';

// Middleware
app.use(helmet({
  contentSecurityPolicy: {
    directives: {
      defaultSrc: ["'self'"],
      scriptSrc: ["'self'", "'unsafe-inline'", "'unsafe-eval'"],
      styleSrc: ["'self'", "'unsafe-inline'"],
      imgSrc: ["'self'", "data:", "https:"],
    },
  },
}));
app.use(compression());
app.use(cors());
app.use(morgan('combined'));
app.use(express.json({ limit: '10mb' }));
app.use(express.urlencoded({ extended: true, limit: '10mb' }));

// Swagger configuration
const swaggerOptions = {
  definition: {
    openapi: '3.0.0',
    info: {
      title: 'ObjectFS Documentation API',
      version: '1.0.0',
      description: 'API for ObjectFS documentation platform features',
    },
    servers: [
      {
        url: `http://localhost:${PORT}`,
        description: 'Development server',
      },
    ],
  },
  apis: ['./src/api-server.js'],
};

const swaggerSpec = swaggerJsdoc(swaggerOptions);
app.use('/api-docs', swaggerUi.serve, swaggerUi.setup(swaggerSpec));

// API Routes

/**
 * @swagger
 * /api/health:
 *   get:
 *     summary: Health check endpoint
 *     responses:
 *       200:
 *         description: API is healthy
 */
app.get('/api/health', (req, res) => {
  res.json({
    status: 'healthy',
    timestamp: new Date().toISOString(),
    version: '1.0.0'
  });
});

/**
 * @swagger
 * /api-playground/{path}:
 *   get:
 *     summary: Proxy GET requests to ObjectFS API
 *     parameters:
 *       - in: path
 *         name: path
 *         required: true
 *         schema:
 *           type: string
 *         description: API path to proxy
 *     responses:
 *       200:
 *         description: Successful response from ObjectFS API
 */
app.get('/api-playground/*', async (req, res) => {
  try {
    const apiPath = req.params[0];
    const queryString = req.url.split('?')[1] || '';
    const url = `${OBJECTFS_API_BASE}/${apiPath}${queryString ? '?' + queryString : ''}`;

    const fetch = (await import('node-fetch')).default;
    const response = await fetch(url, {
      method: req.method,
      headers: {
        'Content-Type': 'application/json',
        ...req.headers
      }
    });

    const data = await response.text();
    let jsonData;
    try {
      jsonData = JSON.parse(data);
    } catch (e) {
      jsonData = data;
    }

    res.status(response.status).json(jsonData);
  } catch (error) {
    res.status(500).json({
      error: 'Proxy error',
      message: error.message,
      note: 'Make sure ObjectFS is running and accessible'
    });
  }
});

/**
 * @swagger
 * /api-playground/{path}:
 *   post:
 *     summary: Proxy POST requests to ObjectFS API
 *     parameters:
 *       - in: path
 *         name: path
 *         required: true
 *         schema:
 *           type: string
 *         description: API path to proxy
 *     requestBody:
 *       required: true
 *       content:
 *         application/json:
 *           schema:
 *             type: object
 *     responses:
 *       200:
 *         description: Successful response from ObjectFS API
 */
app.post('/api-playground/*', async (req, res) => {
  try {
    const apiPath = req.params[0];
    const url = `${OBJECTFS_API_BASE}/${apiPath}`;

    const fetch = (await import('node-fetch')).default;
    const response = await fetch(url, {
      method: req.method,
      headers: {
        'Content-Type': 'application/json',
        ...req.headers
      },
      body: JSON.stringify(req.body)
    });

    const data = await response.text();
    let jsonData;
    try {
      jsonData = JSON.parse(data);
    } catch (e) {
      jsonData = data;
    }

    res.status(response.status).json(jsonData);
  } catch (error) {
    res.status(500).json({
      error: 'Proxy error',
      message: error.message,
      note: 'Make sure ObjectFS is running and accessible'
    });
  }
});

/**
 * @swagger
 * /api/code-runner/execute:
 *   post:
 *     summary: Execute code in a sandboxed environment
 *     requestBody:
 *       required: true
 *       content:
 *         application/json:
 *           schema:
 *             type: object
 *             properties:
 *               language:
 *                 type: string
 *                 enum: [bash, python, javascript, go]
 *               code:
 *                 type: string
 *               timeout:
 *                 type: integer
 *                 default: 30
 *             required:
 *               - language
 *               - code
 *     responses:
 *       200:
 *         description: Code execution result
 */
app.post('/api/code-runner/execute', async (req, res) => {
  const { language, code, timeout = 30 } = req.body;

  if (!language || !code) {
    return res.status(400).json({
      success: false,
      error: 'Language and code are required'
    });
  }

  const supportedLanguages = ['bash', 'python', 'javascript', 'go'];
  if (!supportedLanguages.includes(language)) {
    return res.status(400).json({
      success: false,
      error: `Unsupported language: ${language}`
    });
  }

  try {
    const result = await executeCodeInContainer(language, code, timeout);
    res.json(result);
  } catch (error) {
    res.status(500).json({
      success: false,
      error: error.message
    });
  }
});

/**
 * @swagger
 * /api/tutorials/progress:
 *   post:
 *     summary: Save tutorial progress
 *     requestBody:
 *       required: true
 *       content:
 *         application/json:
 *           schema:
 *             type: object
 *             properties:
 *               tutorialId:
 *                 type: string
 *               stepId:
 *                 type: string
 *               completed:
 *                 type: boolean
 *               data:
 *                 type: object
 *             required:
 *               - tutorialId
 *               - stepId
 *               - completed
 *     responses:
 *       200:
 *         description: Progress saved successfully
 */
app.post('/api/tutorials/progress', (req, res) => {
  // In a real implementation, this would save to a database
  // For demo purposes, we'll just acknowledge the request
  const { tutorialId, stepId, completed, data } = req.body;

  console.log(`Tutorial progress: ${tutorialId}/${stepId} = ${completed}`);

  res.json({
    success: true,
    message: 'Progress saved',
    tutorialId,
    stepId,
    completed,
    timestamp: new Date().toISOString()
  });
});

/**
 * @swagger
 * /api/examples/{category}:
 *   get:
 *     summary: Get code examples for a category
 *     parameters:
 *       - in: path
 *         name: category
 *         required: true
 *         schema:
 *           type: string
 *         description: Example category (e.g., python, javascript, go)
 *     responses:
 *       200:
 *         description: List of code examples
 */
app.get('/api/examples/:category', async (req, res) => {
  const { category } = req.params;

  try {
    const examplesDir = path.join(__dirname, '..', 'examples', category);
    const files = await fs.readdir(examplesDir).catch(() => []);

    const examples = await Promise.all(
      files.map(async (file) => {
        if (!file.endsWith('.js') && !file.endsWith('.py') && !file.endsWith('.go') && !file.endsWith('.sh')) {
          return null;
        }

        try {
          const content = await fs.readFile(path.join(examplesDir, file), 'utf8');
          const lines = content.split('\n');
          const title = lines[0].replace(/^(#|\/\/|\*)/, '').trim();

          return {
            id: file.replace(/\.[^/.]+$/, ""),
            title: title || file,
            filename: file,
            content,
            language: getLanguageFromExtension(file)
          };
        } catch (error) {
          return null;
        }
      })
    );

    res.json({
      category,
      examples: examples.filter(Boolean)
    });
  } catch (error) {
    res.status(500).json({
      error: 'Failed to load examples',
      message: error.message
    });
  }
});

// WebSocket handling for real-time features
io.on('connection', (socket) => {
  console.log('Client connected:', socket.id);

  socket.on('join-tutorial', (tutorialId) => {
    socket.join(`tutorial-${tutorialId}`);
    console.log(`Client ${socket.id} joined tutorial ${tutorialId}`);
  });

  socket.on('tutorial-step', (data) => {
    socket.to(`tutorial-${data.tutorialId}`).emit('peer-progress', {
      socketId: socket.id,
      ...data
    });
  });

  socket.on('code-share', (data) => {
    socket.broadcast.emit('shared-code', {
      socketId: socket.id,
      ...data
    });
  });

  socket.on('disconnect', () => {
    console.log('Client disconnected:', socket.id);
  });
});

// Helper functions

async function executeCodeInContainer(language, code, timeout) {
  const containerConfig = getContainerConfig(language);

  try {
    // Create container
    const container = await docker.createContainer({
      Image: containerConfig.image,
      Cmd: containerConfig.getCommand(code),
      WorkingDir: '/workspace',
      HostConfig: {
        Memory: 128 * 1024 * 1024, // 128MB
        CpuQuota: 50000, // 50% CPU
        NetworkMode: 'none', // No network access
        ReadonlyRootfs: true,
        Tmpfs: {
          '/tmp': 'rw,noexec,nosuid,size=32m',
          '/workspace': 'rw,noexec,nosuid,size=64m'
        }
      },
      AttachStdout: true,
      AttachStderr: true,
    });

    // Start container
    await container.start();

    // Get output with timeout
    const stream = await container.logs({
      stdout: true,
      stderr: true,
      follow: true
    });

    let output = '';
    let error = '';

    const timeoutPromise = new Promise((_, reject) => {
      setTimeout(() => reject(new Error('Execution timeout')), timeout * 1000);
    });

    const execPromise = new Promise((resolve) => {
      stream.on('data', (chunk) => {
        const data = chunk.toString();
        if (data.includes('stderr')) {
          error += data;
        } else {
          output += data;
        }
      });

      stream.on('end', () => {
        resolve();
      });
    });

    await Promise.race([execPromise, timeoutPromise]);

    // Clean up
    await container.remove({ force: true });

    return {
      success: true,
      output: cleanOutput(output),
      error: cleanOutput(error) || null
    };
  } catch (error) {
    return {
      success: false,
      error: error.message
    };
  }
}

function getContainerConfig(language) {
  const configs = {
    bash: {
      image: 'alpine:latest',
      getCommand: (code) => ['sh', '-c', code]
    },
    python: {
      image: 'python:3.11-alpine',
      getCommand: (code) => ['python', '-c', code]
    },
    javascript: {
      image: 'node:18-alpine',
      getCommand: (code) => ['node', '-e', code]
    },
    go: {
      image: 'golang:1.21-alpine',
      getCommand: (code) => ['go', 'run', '-']
    }
  };

  return configs[language];
}

function cleanOutput(output) {
  // Remove Docker log prefixes and clean up output
  return output
    .replace(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d+Z\s+/gm, '')
    .replace(/^stdout\s+/gm, '')
    .replace(/^stderr\s+/gm, '')
    .trim();
}

function getLanguageFromExtension(filename) {
  const ext = path.extname(filename).toLowerCase();
  const mapping = {
    '.js': 'javascript',
    '.py': 'python',
    '.go': 'go',
    '.sh': 'bash'
  };
  return mapping[ext] || 'text';
}

// Error handling
app.use((err, req, res, next) => {
  console.error('Unhandled error:', err);
  res.status(500).json({
    error: 'Internal server error',
    message: process.env.NODE_ENV === 'development' ? err.message : 'Something went wrong'
  });
});

// 404 handler
app.use((req, res) => {
  res.status(404).json({
    error: 'Not found',
    path: req.path
  });
});

// Start server
server.listen(PORT, () => {
  console.log(`ObjectFS Documentation API Server running on port ${PORT}`);
  console.log(`API docs available at http://localhost:${PORT}/api-docs`);
  console.log(`Environment: ${process.env.NODE_ENV || 'development'}`);
});

// Graceful shutdown
process.on('SIGTERM', () => {
  console.log('SIGTERM received, shutting down gracefully');
  server.close(() => {
    console.log('Server closed');
    process.exit(0);
  });
});

process.on('SIGINT', () => {
  console.log('SIGINT received, shutting down gracefully');
  server.close(() => {
    console.log('Server closed');
    process.exit(0);
  });
});
