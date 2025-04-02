package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Product model
type Product struct {
	ID          uint      `json:"id" gorm:"primaryKey"`
	Name        string    `json:"name" binding:"required"`
	Description string    `json:"description"`
	Price       float64   `json:"price" binding:"required"`
	ImagePath   string    `json:"image_path"`
	CreatedAt   time.Time `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt   time.Time `json:"updated_at" gorm:"autoUpdateTime"`
}

var db *gorm.DB

// Memory pool for byte buffers to reduce GC pressure
var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func main() {
	// Set gin to release mode for production
	gin.SetMode(gin.ReleaseMode)

	// Initialize database
	initDB()

	// Ensure upload directory exists
	uploadDir := "./uploads"
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatalf("Failed to create upload directory: %v", err)
	}

	// Set up Gin router
	router := gin.Default()
	
	// Configure CORS
	router.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Content-Length", "Accept-Encoding", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	// Serve static files
	router.Static("/uploads", uploadDir)
	router.MaxMultipartMemory = 8 << 20 // 8 MiB

	// API routes
	router.POST("/products", createProduct)
	router.GET("/products", getProducts)
	router.GET("/products/:id", getProduct)
	router.PUT("/products/:id", updateProduct)
	router.DELETE("/products/:id", deleteProduct)

	// Health check endpoint
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Start server on port 3006
	log.Println("Server starting on port 3006...")
	router.Run(":3006")
}

func initDB() {
	// Get database connection details from environment variables with defaults
	dbUser := getEnv("DB_USER", "postgres")
	dbPassword := getEnv("DB_PASSWORD", "123")
	dbHost := getEnv("DB_HOST", "localhost")
	dbPort := getEnv("DB_PORT", "5432")
	dbName := getEnv("DB_NAME", "productdb")
	
	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=Asia/Jakarta",
		dbHost, dbUser, dbPassword, dbName, dbPort)
	
	// Custom logger configuration
	newLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		logger.Config{
			SlowThreshold:             time.Second,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)
	
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: newLogger,
		PrepareStmt: true, // Cache prepared statements for better performance
	})
	
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	
	// Migrate the schema
	err = db.AutoMigrate(&Product{})
	if err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}
	
	// Optimize database connection pool
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("Failed to get DB instance: %v", err)
	}
	
	// Optimized connection pool settings
	sqlDB.SetMaxIdleConns(20)
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
}

// Helper function to get environment variables with fallback to default value
func getEnv(key, fallback string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		return fallback
	}
	return value
}

// Create a new product
func createProduct(c *gin.Context) {
	// Start timer for performance monitoring
	startTime := time.Now()
	
	// Parse multipart form
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse form"})
		return
	}
	
	// Extract product details
	name := form.Value["name"][0]
	description := ""
	if len(form.Value["description"]) > 0 {
		description = form.Value["description"][0]
	}
	
	priceStr := form.Value["price"][0]
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid price format"})
		return
	}
	
	// Create product instance
	product := Product{
		Name:        name,
		Description: description,
		Price:       price,
	}
	
	// Handle image upload if present
	files := form.File["image"]
	if len(files) > 0 {
		file := files[0]
		
		
		// Generate unique filename
		ext := filepath.Ext(file.Filename)
		filename := uuid.New().String() + ext
		imagePath := filepath.Join("uploads", filename)
		
		// Process and save the image
		src, err := file.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open uploaded file"})
			return
		}
		defer src.Close()
		
		// Process image (resize and compress)
		optimizedImagePath, err := optimizeImage(src, imagePath, ext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process image"})
			return
		}
		
		// Update product with image path
		product.ImagePath = optimizedImagePath
	}
	
	// Save to database
	result := db.Create(&product)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create product"})
		return
	}
	
	// Log performance
	elapsed := time.Since(startTime).Milliseconds()
	log.Printf("Product created in %dms", elapsed)
	
	// Return created product
	c.JSON(http.StatusCreated, product)
}

// Get all products with pagination
func getProducts(c *gin.Context) {
	var products []Product
	
	// Parse pagination parameters
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "10"))
	
	// Validate pagination parameters
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}
	
	// Calculate offset
	offset := (page - 1) * pageSize
	
	// Get total count
	var total int64
	db.Model(&Product{}).Count(&total)
	
	// Query with pagination
	if err := db.Offset(offset).Limit(pageSize).Find(&products).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch products"})
		return
	}
	
	// Return response with pagination metadata
	c.JSON(http.StatusOK, gin.H{
		"data":       products,
		"total":      total,
		"page":       page,
		"page_size":  pageSize,
		"total_pages": (int(total) + pageSize - 1) / pageSize,
	})
}

// Get a single product by ID
func getProduct(c *gin.Context) {
	id := c.Param("id")
	var product Product
	
	if err := db.First(&product, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	
	c.JSON(http.StatusOK, product)
}

// Update a product
func updateProduct(c *gin.Context) {
	startTime := time.Now()
	id := c.Param("id")
	
	// Find existing product
	var product Product
	if err := db.First(&product, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	
	// Parse multipart form
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse form"})
		return
	}
	
	// Update product fields if provided
	if len(form.Value["name"]) > 0 {
		product.Name = form.Value["name"][0]
	}
	
	if len(form.Value["description"]) > 0 {
		product.Description = form.Value["description"][0]
	}
	
	if len(form.Value["price"]) > 0 {
		price, err := strconv.ParseFloat(form.Value["price"][0], 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid price format"})
			return
		}
		product.Price = price
	}
	
	// Handle new image upload if present
	files := form.File["image"]
	if len(files) > 0 {
		file := files[0]
		
	
		
		// Delete old image if exists
		if product.ImagePath != "" {
			// Delete in goroutine to not block the response
			oldPath := product.ImagePath
			go func() {
				os.Remove(oldPath)
			}()
		}
		
		// Generate unique filename
		ext := filepath.Ext(file.Filename)
		filename := uuid.New().String() + ext
		imagePath := filepath.Join("uploads", filename)
		
		// Process and save the image
		src, err := file.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open uploaded file"})
			return
		}
		defer src.Close()
		
		// Process image (resize and compress)
		optimizedImagePath, err := optimizeImage(src, imagePath, ext)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to process image"})
			return
		}
		
		// Update product with new image path
		product.ImagePath = optimizedImagePath
	}
	
	// Save updates to database
	if err := db.Save(&product).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update product"})
		return
	}
	
	// Log performance
	elapsed := time.Since(startTime).Milliseconds()
	log.Printf("Product updated in %dms", elapsed)
	
	c.JSON(http.StatusOK, product)
}

// Delete a product
func deleteProduct(c *gin.Context) {
	id := c.Param("id")
	
	// Find product
	var product Product
	if err := db.First(&product, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Product not found"})
		return
	}
	
	// Delete image file if exists in a goroutine
	if product.ImagePath != "" {
		imagePath := product.ImagePath
		go func() {
			os.Remove(imagePath)
		}()
	}
	
	// Delete from database
	if err := db.Delete(&product).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete product"})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{"message": "Product deleted successfully"})
}

// Helper function to optimize image by resizing and compressing
func optimizeImage(src io.Reader, destPath string, ext string) (string, error) {
	// Get buffer from pool
	buffer := bufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	defer bufferPool.Put(buffer)
	
	// Read the image
	_, err := io.Copy(buffer, src)
	if err != nil {
		return "", err
	}
	
	// Create a channel for results
	resultCh := make(chan struct {
		path string
		err  error
	})
	
	// Process image in a goroutine
	go func() {
		// Decode image
		imgSrc, _, err := image.Decode(bytes.NewReader(buffer.Bytes()))
		if err != nil {
			resultCh <- struct {
				path string
				err  error
			}{"", err}
			return
		}
		
		// Resize image to max dimensions while preserving aspect ratio
		// Reduced dimensions for faster processing
		maxWidth := 800
		maxHeight := 800
		
		// Use faster scaling algorithm (Box instead of Lanczos)
		imgResized := imaging.Fit(imgSrc, maxWidth, maxHeight, imaging.Box)
		
		// Create destination file
		out, err := os.Create(destPath)
		if err != nil {
			resultCh <- struct {
				path string
				err  error
			}{"", err}
			return
		}
		defer out.Close()
		
		// Encoding options based on file type - optimized for speed
		switch strings.ToLower(ext) {
		case ".jpg", ".jpeg":
			// Lower quality for faster processing (80% instead of 85%)
			err = jpeg.Encode(out, imgResized, &jpeg.Options{Quality: 80})
		case ".png":
			// Use default compression for better speed instead of BestCompression
			encoder := png.Encoder{CompressionLevel: png.DefaultCompression}
			err = encoder.Encode(out, imgResized)
		default:
			// Default to JPEG if not recognized
			err = jpeg.Encode(out, imgResized, &jpeg.Options{Quality: 80})
		}
		
		if err != nil {
			resultCh <- struct {
				path string
				err  error
			}{"", err}
			return
		}
		
		resultCh <- struct {
			path string
			err  error
		}{destPath, nil}
	}()
	
	// Wait for result with timeout
	select {
	case result := <-resultCh:
		return result.path, result.err
	case <-time.After(10 * time.Second):
		return "", fmt.Errorf("image processing timed out")
	}
}