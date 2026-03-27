FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY src/ src/
RUN npx tsc

FROM node:22-alpine
RUN apk add --no-cache curl
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY --from=build /app/dist/ dist/
COPY public/ public/
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=10s --retries=5 \
  CMD ["curl", "-f", "http://localhost:3000/readyz"]
CMD ["node", "dist/server.js"]
