FROM node:20-alpine
LABEL org.opencontainers.image.title="llmmcp" \
      org.opencontainers.image.description="LLM-MCP HTTP bridge service" \
      org.opencontainers.image.source="https://github.com/plagness/LLM-MCP.git" \
      ns.module="llm" \
      ns.component="mcp"
WORKDIR /app
COPY package.json tsconfig.json ./
RUN npm install
COPY src ./src
COPY proto ./proto
RUN npm run build
EXPOSE 3333
CMD ["node", "dist/index.js"]
